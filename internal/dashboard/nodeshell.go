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

// The generateName the SPA's NodeShell pod uses.
const nodeShellPrefix = "stratum-node-shell-"

// Pod/Job creates and prefixed exec only; a hostPID DaemonSet is the caller's RBAC, as via kubectl.
func (p *proxy) checkNodeShell(w http.ResponseWriter, r *http.Request, path string) bool {
	if p.nodeShell {
		return false
	}
	req, ok := parseK8sPath(path)
	if !ok {
		return false
	}
	isJob := req.Group == "batch" && req.Resource == "jobs"
	if !isJob && (req.Group != "" || req.Resource != "pods") {
		return false
	}

	switch req.Subresource {
	case "exec", "attach":
		// attach lands on the same shell: the shell container runs with stdin and a tty.
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
	// Fail closed on unreadable bodies: YAML is a JSON superset, protobuf is refused.
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
			// hostPID is what makes nsenter a host shell; renaming must not be enough to get one.
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
		// hostPID is what makes nsenter a host shell; renaming must not be enough to get one.
		p.refuse(w, r, "creating a pod with hostPID is disabled; start the dashboard with --enable-node-shell to allow it")
		return true
	}
	return false
}

// Matches the apiserver's case-sensitive decoding; encoding/json folds case, last key wins.
func decodeGatedBody(body []byte, v any) error {
	return kjson.UnmarshalCaseSensitivePreserveInts(body, v)
}

type gatedPod struct {
	Metadata struct {
		GenerateName string `json:"generateName"`
		Name         string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		HostPID bool `json:"hostPID"`
	} `json:"spec"`
}

// A PATCH is read by content type; a json-patch op aimed at hostPID is judged as setting it.
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

// Unescape ~1 before ~0 (RFC 6901); a raw match would let /spec~1hostPID through.
func unescapeJSONPointer(p string) string {
	p = strings.ReplaceAll(p, "~1", "/")
	return strings.ReplaceAll(p, "~0", "~")
}

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

// Same patch rules as gatedPodFrom, applied to the Job template.
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

// Logged: a gate that fires silently is indistinguishable from a feature nobody used.
func (p *proxy) refuse(w http.ResponseWriter, r *http.Request, msg string) {
	p.log.Warn("node shell refused", "id", requestID(r.Context()), "actor", actorFrom(r.Context()),
		"method", r.Method, "path", r.URL.Path, "remote", remoteIP(r))
	writeStatus(w, http.StatusForbidden, "Forbidden", msg, "node-shell-disabled")
}

// Reports false when it has already written a response.
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
