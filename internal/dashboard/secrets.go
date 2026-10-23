package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	secretSummariesPath = "/api/v1/secret-summaries"
	tableAccept         = "application/json;as=Table;g=meta.k8s.io;v=v1"
	helmReleaseType     = "helm.sh/release.v1"
)

// secretSummary is everything the SPA's live load keeps of a Secret.
type secretSummary struct {
	Name              string       `json:"name"`
	Namespace         string       `json:"namespace"`
	Type              string       `json:"type"`
	Keys              int64        `json:"keys"`
	CreationTimestamp metav1.Time  `json:"creationTimestamp"`
	Helm              *helmRelease `json:"helm,omitempty"`
}

// helmRelease is the release identity Helm puts in a release Secret's labels.
type helmRelease struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Status  string `json:"status"`
}

// secretTable is the part of a meta.k8s.io/v1 Table of Secrets this reads.
type secretTable struct {
	ColumnDefinitions []struct {
		Name string `json:"name"`
	} `json:"columnDefinitions"`
	Rows []struct {
		Cells  []json.RawMessage `json:"cells"`
		Object struct {
			Metadata metav1.ObjectMeta `json:"metadata"`
		} `json:"object"`
	} `json:"rows"`
}

// secretSummaries lists every Secret the caller may list, cluster-wide, as
// the session's own user, and answers with name, namespace, type, key count,
// age and Helm release labels only.
func (s *server) secretSummaries(w http.ResponseWriter, r *http.Request) {
	token, _ := tokenFrom(r.Context())
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	items, err := s.listSecretSummaries(ctx, token)
	if err != nil {
		var st apierrors.APIStatus
		if errors.As(err, &st) && st.Status().Code != 0 {
			writeJSONError(w, int(st.Status().Code), "upstream", st.Status().Message)
			return
		}
		s.log.Warn("secret summaries", "id", requestID(r.Context()), "err", err)
		writeJSONError(w, http.StatusBadGateway, "upstream-error", "the apiserver could not be reached")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *server) listSecretSummaries(ctx context.Context, token string) ([]secretSummary, error) {
	cfg := rest.AnonymousClientConfig(s.base)
	cfg.BearerToken = token
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	raw, err := client.CoreV1().RESTClient().Get().
		Resource("secrets").
		Param("includeObject", string(metav1.IncludeMetadata)).
		SetHeader("Accept", tableAccept).
		DoRaw(ctx)
	if err != nil {
		return nil, err
	}
	return summarizeSecretTable(raw)
}

func summarizeSecretTable(raw []byte) ([]secretSummary, error) {
	var t secretTable
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, err
	}
	col := map[string]int{}
	for i, c := range t.ColumnDefinitions {
		col[c.Name] = i
	}
	cell := func(cells []json.RawMessage, name string, into any) {
		if i, ok := col[name]; ok && i < len(cells) {
			_ = json.Unmarshal(cells[i], into)
		}
	}

	out := make([]secretSummary, 0, len(t.Rows))
	for _, row := range t.Rows {
		m := row.Object.Metadata
		sum := secretSummary{Name: m.Name, Namespace: m.Namespace, CreationTimestamp: m.CreationTimestamp}
		cell(row.Cells, "Type", &sum.Type)
		cell(row.Cells, "Data", &sum.Keys)
		if sum.Type == helmReleaseType {
			sum.Helm = &helmRelease{Name: m.Labels["name"], Version: m.Labels["version"], Status: m.Labels["status"]}
		}
		out = append(out, sum)
	}
	return out, nil
}
