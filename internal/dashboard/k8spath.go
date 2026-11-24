package dashboard

import "strings"

// The legacy watch form inserts a segment after the version, so parse, do not position-match.

// k8sRequest is one parsed Kubernetes API path.
type k8sRequest struct {
	Group   string
	Version string
	// Not the HTTP method: it marks the legacy /watch/ path form only.
	Verb        string
	Namespace   string
	Resource    string
	Name        string
	Subresource string
}

// ok=false means not a Kubernetes resource request (/version, /openapi), not an error.
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
