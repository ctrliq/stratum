package dashboard

import "strings"

// Request paths are parsed rather than matched by position because the
// deprecated-but-still-served watch form inserts a segment right after the
// version: a positional matcher can read
// /api/v1/watch/namespaces/x/secrets as a request for a resource called
// "watch" and let it through. The node-shell gate below makes the same kind of
// decision about the same kind of path, so it uses the same parser.
//
// The shapes the API server serves, per
// k8s.io/apiserver/pkg/endpoints/installer.go:
//
//	/api/v1/{resource}[/{name}[/{subresource}]]
//	/api/v1/namespaces/{ns}/{resource}[/{name}[/{subresource}]]
//	/apis/{group}/{version}/{resource}[/{name}[/{subresource}]]
//	/apis/{group}/{version}/namespaces/{ns}/{resource}[/{name}[/{subresource}]]
//
// each also reachable as /api/v1/watch/... or /api/v1/watch/namespaces/{ns}/...

// k8sRequest is one parsed Kubernetes API path.
type k8sRequest struct {
	// Group is "" for the core group (/api/v1/...).
	Group   string
	Version string
	// Verb is "watch" when the path used the legacy watch form, otherwise "".
	// It is not the HTTP method; callers still need that separately.
	Verb string
	// Namespace is "" for cluster-scoped or all-namespace requests.
	Namespace   string
	Resource    string
	Name        string
	Subresource string
}

// parseK8sPath splits a Kubernetes API path already stripped of the proxy
// prefix. ok=false means "not an /api or /apis resource request" (/version,
// /openapi, /healthz), which callers treat as "nothing addressed here" rather
// than as an error.
func parseK8sPath(path string) (k8sRequest, bool) {
	p := strings.Trim(path, "/")
	if p == "" {
		return k8sRequest{}, false
	}
	seg := strings.Split(p, "/")

	var r k8sRequest
	switch seg[0] {
	case "api":
		if len(seg) < 3 {
			return k8sRequest{}, false
		}
		r.Version = seg[1]
		seg = seg[2:]
	case "apis":
		if len(seg) < 4 {
			return k8sRequest{}, false
		}
		r.Group, r.Version = seg[1], seg[2]
		seg = seg[3:]
	default:
		return k8sRequest{}, false
	}

	// Strip the watch segment and let the rest of the parse proceed
	// identically. This is the line whose absence makes a deny-list
	// bypassable.
	if seg[0] == "watch" {
		r.Verb = "watch"
		seg = seg[1:]
		if len(seg) == 0 {
			return k8sRequest{}, false
		}
	}

	if len(seg) >= 3 && seg[0] == "namespaces" {
		r.Namespace = seg[1]
		seg = seg[2:]
	}
	if len(seg) == 0 {
		return k8sRequest{}, false
	}

	r.Resource = seg[0]
	if len(seg) > 1 {
		r.Name = seg[1]
	}
	if len(seg) > 2 {
		r.Subresource = seg[2]
	}
	return r, true
}
