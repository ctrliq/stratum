package dashboard

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	kjson "sigs.k8s.io/json"
	"sigs.k8s.io/yaml"
)

// nodeShellPrefix is the generateName the SPA's NodeShell uses. It is a
// hostPID, privileged pod that execs `nsenter -t 1 -m -u -i -n -p -- sh`:
// a root shell on the node, which is why the server refuses it here when
// --enable-node-shell=false, not only in the UI.
const nodeShellPrefix = "stratum-node-shell-"

// checkNodeShell refuses, before anything is forwarded, the requests that
// together make a host shell — but only while --enable-node-shell is off. It
// reports whether the request was refused.
//
// This is the server-side half of the gate, so someone driving /k8s/ with curl
// gets the same refusal the SPA does; /api/v1/config reports the flag purely
// so the SPA can hide a control it knows will fail, which is convenience, not
// enforcement. The other half is the caller's own RBAC: even with the flag on,
// the apiserver still has to grant that user jobs create, jobs delete and
// pods/exec create, and the dashboard never grants what the token lacks.
//
// A create is a POST to the pods collection or to the batch jobs collection,
// or a PATCH to a named one: a server-side apply creates the object when it
// does not exist, so a PATCH body is judged the same way a POST body is. The
// shell itself is a batch/v1 Job whose pod template is the host shell — the
// TTL controller deletes the finished Job — so a Job create or patch is judged
// by its template, the pod it would create, exactly as a pod body is judged.
// A Job's pod inherits the Job's generateName plus a suffix, so exec/attach
// keep matching both the bare pod and the Job-derived pod by one prefix.
//
// The gate covers direct pod and Job creates. Someone whose RBAC lets them
// create a DaemonSet with hostPID can reach the host that way, flag or no
// flag — that is their own RBAC's decision to make, and the same is true
// through kubectl.
func (p *proxy) checkNodeShell(w http.ResponseWriter, r *http.Request, path string) bool {
	if p.nodeShell {
		return false
	}
	req, ok := parseK8sPath(path)
	if !ok {
		return false
	}
	// Pods are the gate's original subject; the batch Job is the same request
	// wrapped in a Job. Every other group and resource is outside this
	// gate, for the DaemonSet reason below.
	isJob := req.Group == "batch" && req.Resource == "jobs"
	if !isJob && (req.Group != "" || req.Resource != "pods") {
		return false
	}

	switch req.Subresource {
	case "exec", "attach":
		// attach as well as exec: the shell container runs with stdin and a
		// tty, so attaching to it lands on the same root shell exec would.
		if strings.HasPrefix(req.Name, nodeShellPrefix) {
			p.refuse(w, r, "exec into a "+nodeShellPrefix+" pod is disabled; start the dashboard with --enable-node-shell to allow the node shell")
			return true
		}
		return false
	case "":
		switch {
		case r.Method == http.MethodPost && req.Name == "":
		case r.Method == http.MethodPatch && req.Name != "":
			if strings.HasPrefix(req.Name, nodeShellPrefix) {
				p.refuse(w, r, "the node shell is disabled; start the dashboard with --enable-node-shell to allow it")
				return true
			}
		default:
			return false
		}
	default:
		return false
	}

	body, ok := drainBody(w, r)
	if !ok {
		return true
	}
	// Fail closed on anything this cannot read. The apiserver also accepts
	// YAML and protobuf pod and Job bodies, and a gate that waved through what
	// it could not inspect would be the bypass rather than the gate. YAML is a
	// superset of JSON, so a JSON body sent as application/yaml still parses
	// and is still judged; protobuf does not, and is refused. kubectl and the
	// SPA both send JSON.
	if isJob {
		job, err := gatedJobFrom(r, body)
		if err != nil {
			p.refuse(w, r, "the Job body could not be parsed, so it cannot be checked against the node-shell gate")
			return true
		}
		switch {
		case strings.HasPrefix(job.Spec.Template.Metadata.GenerateName, nodeShellPrefix),
			strings.HasPrefix(job.Spec.Template.Metadata.Name, nodeShellPrefix):
			p.refuse(w, r, "the node shell is disabled; start the dashboard with --enable-node-shell to allow it")
			return true
		case job.Spec.Template.Spec.HostPID:
			// Broader than the node shell by name, deliberately: hostPID is
			// the property that makes `nsenter -t 1` a host shell, and
			// renaming the template must not be enough to get one.
			p.refuse(w, r, "creating a Job whose pod template sets hostPID is disabled; start the dashboard with --enable-node-shell to allow it")
			return true
		}
		return false
	}
	pod, err := gatedPodFrom(r, body)
	if err != nil {
		p.refuse(w, r, "the pod body could not be parsed, so it cannot be checked against the node-shell gate")
		return true
	}
	switch {
	case strings.HasPrefix(pod.Metadata.GenerateName, nodeShellPrefix), strings.HasPrefix(pod.Metadata.Name, nodeShellPrefix):
		p.refuse(w, r, "the node shell is disabled; start the dashboard with --enable-node-shell to allow it")
		return true
	case pod.Spec.HostPID:
		// Broader than the node shell by name, deliberately: hostPID is the
		// property that makes `nsenter -t 1` a host shell, and renaming the
		// pod must not be enough to get one.
		p.refuse(w, r, "creating a pod with hostPID is disabled; start the dashboard with --enable-node-shell to allow it")
		return true
	}
	return false
}

// decodeGatedBody decodes a body the gate judges with the same decoder the
// apiserver's JSON serializer uses: keys match case-sensitively, and a key that
// differs only in case is an unknown field and is dropped. encoding/json folds
// case and lets the last match win, so a body carrying both "hostPID" and
// "HOSTPID" would be read one way here and the other way by the apiserver.
func decodeGatedBody(body []byte, v any) error {
	return kjson.UnmarshalCaseSensitivePreserveInts(body, v)
}

// gatedPod is the part of a pod body the gate judges.
type gatedPod struct {
	Metadata struct {
		GenerateName string `json:"generateName"`
		Name         string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		HostPID bool `json:"hostPID"`
	} `json:"spec"`
}

// gatedPodFrom reads the pod a POST or PATCH body would produce. A PATCH is
// read by its content type, and any content type the apiserver could apply
// that is not listed here is an error, so it is refused rather than forwarded.
// A JSON patch is a list of operations rather than a pod, so an operation that
// could write hostPID — one aimed at the document, at spec, or at
// spec.hostPID — is judged as if it set it.
func gatedPodFrom(r *http.Request, body []byte) (gatedPod, error) {
	var pod gatedPod
	if r.Method != http.MethodPatch {
		return pod, decodeGatedBody(body, &pod)
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return pod, err
	}
	switch mediaType {
	case "application/apply-patch+yaml":
		j, err := yaml.YAMLToJSON(body)
		if err != nil {
			return pod, err
		}
		return pod, decodeGatedBody(j, &pod)
	case "application/merge-patch+json", "application/strategic-merge-patch+json":
		return pod, decodeGatedBody(body, &pod)
	case "application/json-patch+json":
		var ops []struct {
			Path string `json:"path"`
		}
		if err := decodeGatedBody(body, &ops); err != nil {
			return pod, err
		}
		for _, op := range ops {
			switch unescapeJSONPointer(op.Path) {
			case "", "/spec", "/spec/hostPID":
				pod.Spec.HostPID = true
			}
		}
		return pod, nil
	}
	return pod, errors.New("unsupported patch content type " + mediaType)
}

// unescapeJSONPointer decodes the escape a JSON Pointer's reference tokens use
// (RFC 6901): ~1 is a literal slash and ~0 a literal tilde, in that order. The
// apiserver applies an application/json-patch+json path after this decode, so
// matching the raw path lets `/spec~1hostPID` — a path aimed at spec.hostPID
// as surely as the unescaped spelling — forward as if it touched something
// else; a gate that judges a patch must judge the pointer the patch applies.
func unescapeJSONPointer(p string) string {
	p = strings.ReplaceAll(p, "~1", "/")
	return strings.ReplaceAll(p, "~0", "~")
}

// gatedJob is the part of a Job body the gate judges: the pod template the
// Job would create, which is where a host shell hides.
type gatedJob struct {
	Spec struct {
		Template struct {
			Metadata struct {
				GenerateName string `json:"generateName"`
				Name         string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				HostPID bool `json:"hostPID"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

// gatedJobFrom reads the pod a Job POST or PATCH body would produce, by the
// same rules gatedPodFrom reads a pod body: a PATCH is read by its content
// type, and any content type the apiserver could apply that is not listed here
// is an error, so it is refused rather than forwarded. A JSON patch is a list
// of operations rather than a Job, so an operation that could write the
// template's hostPID — one aimed at the document, at spec, at the template,
// or at the template's spec or spec.hostPID — is judged as if it set it.
func gatedJobFrom(r *http.Request, body []byte) (gatedJob, error) {
	var job gatedJob
	if r.Method != http.MethodPatch {
		return job, decodeGatedBody(body, &job)
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return job, err
	}
	switch mediaType {
	case "application/apply-patch+yaml":
		j, err := yaml.YAMLToJSON(body)
		if err != nil {
			return job, err
		}
		return job, decodeGatedBody(j, &job)
	case "application/merge-patch+json", "application/strategic-merge-patch+json":
		return job, decodeGatedBody(body, &job)
	case "application/json-patch+json":
		var ops []struct {
			Path string `json:"path"`
		}
		if err := decodeGatedBody(body, &ops); err != nil {
			return job, err
		}
		for _, op := range ops {
			switch unescapeJSONPointer(op.Path) {
			case "", "/spec", "/spec/template", "/spec/template/spec", "/spec/template/spec/hostPID":
				job.Spec.Template.Spec.HostPID = true
			}
		}
		return job, nil
	}
	return job, errors.New("unsupported patch content type " + mediaType)
}

// refuse answers 403 and logs it: a gate that leaves no trace when it fires
// cannot be told apart from a feature nobody used.
func (p *proxy) refuse(w http.ResponseWriter, r *http.Request, msg string) {
	p.log.Warn("node shell refused", "id", requestID(r.Context()), "actor", actorFrom(r.Context()),
		"method", r.Method, "path", r.URL.Path, "remote", remoteIP(r))
	writeStatus(w, http.StatusForbidden, "Forbidden", msg, "node-shell-disabled")
}

// drainBody reads the request body so it can be inspected and puts it back for
// forwarding. It reports false when it has already answered.
func drainBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, true
	}
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeStatus(w, http.StatusRequestEntityTooLarge, "RequestEntityTooLarge", "request body exceeds 4 MiB", "body-too-large")
			return nil, false
		}
		writeStatus(w, http.StatusBadRequest, "BadRequest", "could not read the request body", "bad-request")
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	return body, true
}
