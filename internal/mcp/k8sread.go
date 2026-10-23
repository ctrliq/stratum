package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/ctrliq/stratum/internal/mcp/kube"
)

// The generic Kubernetes read toolset.

// Result bounds. A tool's answer lands in a model's context window, so these
// are sized for that rather than for what the apiserver could serve.
const (
	// DefaultListLimit is how many items a list tool returns when the caller
	// does not ask for a specific number.
	DefaultListLimit = 100
	// MaxListLimit is the most any list tool will return, whatever was asked
	// for. A caller wanting more pages through Continue.
	MaxListLimit = 500

	// DefaultLogLines and MaxLogBytes bound pod_logs. Logs are the one read
	// here with no natural item count, so they are capped twice.
	DefaultLogLines = 200
	MaxLogBytes     = 256 << 10
)

// ListMeta is the envelope every list tool returns, and the shape the other
// list tools copy.
type ListMeta struct {
	Count     int    `json:"count" jsonschema:"how many items this response contains"`
	Limit     int    `json:"limit" jsonschema:"the maximum this call would have returned"`
	Truncated bool   `json:"truncated" jsonschema:"true when the cluster holds more items than were returned; the result is a partial view"`
	Continue  string `json:"continue,omitempty" jsonschema:"opaque token: pass it back as continue_token to fetch the next page"`
	Note      string `json:"note,omitempty" jsonschema:"what to do about a truncated result"`
}

// newListMeta builds the envelope from what a list call returned.
func newListMeta(count, limit int, continueToken string) ListMeta {
	m := ListMeta{Count: count, Limit: limit, Continue: continueToken}
	if continueToken != "" {
		m.Truncated = true
		m.Note = fmt.Sprintf("Showing %d items; more exist. Pass continue_token to page, "+
			"or narrow with a namespace or label_selector.", count)
	}
	return m
}

// ListOptions is the input every list tool embeds.
type ListOptions struct {
	Context       string `json:"context,omitempty" jsonschema:"kubeconfig context to address; omit for the current one"`
	Namespace     string `json:"namespace,omitempty" jsonschema:"namespace to search; omit with all_namespaces for every namespace"`
	AllNamespaces bool   `json:"all_namespaces,omitempty" jsonschema:"search every namespace instead of one"`
	LabelSelector string `json:"label_selector,omitempty" jsonschema:"label selector, e.g. app=nginx,tier!=backend"`
	FieldSelector string `json:"field_selector,omitempty" jsonschema:"field selector, e.g. status.phase=Running"`
	Limit         int    `json:"limit,omitempty" jsonschema:"maximum items to return; defaults to 100, capped at 500"`
	ContinueToken string `json:"continue_token,omitempty" jsonschema:"token from a previous truncated response"`
}

// limit clamps a requested limit into the allowed range.
func (o ListOptions) limit() int {
	switch {
	case o.Limit <= 0:
		return DefaultListLimit
	case o.Limit > MaxListLimit:
		return MaxListLimit
	default:
		return o.Limit
	}
}

func (o ListOptions) namespace() string {
	if o.AllNamespaces {
		return metav1.NamespaceAll
	}
	return o.Namespace
}

func (o ListOptions) meta() metav1.ListOptions {
	return metav1.ListOptions{
		LabelSelector: o.LabelSelector,
		FieldSelector: o.FieldSelector,
		Limit:         int64(o.limit()),
		Continue:      o.ContinueToken,
	}
}

// ObjectRef is the common shape of "which single object".
type ObjectRef struct {
	Context   string `json:"context,omitempty" jsonschema:"kubeconfig context to address; omit for the current one"`
	Name      string `json:"name" jsonschema:"the object's name"`
	Namespace string `json:"namespace,omitempty" jsonschema:"the object's namespace; omit for cluster-scoped objects"`
}

// ObjectSummary is the per-item shape of a generic list.
type ObjectSummary struct {
	Name       string            `json:"name"`
	Namespace  string            `json:"namespace,omitempty"`
	Kind       string            `json:"kind,omitempty"`
	APIVersion string            `json:"apiVersion,omitempty"`
	Created    string            `json:"created,omitempty" jsonschema:"RFC 3339 creation timestamp"`
	Age        string            `json:"age,omitempty" jsonschema:"human-readable age, e.g. 4d2h"`
	Labels     map[string]string `json:"labels,omitempty"`
}

func summarize(item *unstructured.Unstructured) ObjectSummary {
	s := ObjectSummary{
		Name:       item.GetName(),
		Namespace:  item.GetNamespace(),
		Kind:       item.GetKind(),
		APIVersion: item.GetAPIVersion(),
		Labels:     item.GetLabels(),
	}
	if ts := item.GetCreationTimestamp(); !ts.IsZero() {
		s.Created = ts.UTC().Format(time.RFC3339)
		s.Age = age(ts.Time)
	}
	return s
}

// age renders a duration the way kubectl does, because that is the form
// every operator already reads without thinking.
func age(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// cluster resolves the context argument to a connected cluster.
func cluster(conn *kube.Connector, name string) (*kube.Cluster, error) {
	cl, err := conn.Cluster(name)
	if err != nil {
		return nil, fmt.Errorf("connect to cluster: %w", err)
	}
	return cl, nil
}

// ---------------------------------------------------------------- resources

// ResourceListIn is resource_list's input.
type ResourceListIn struct {
	ListOptions
	Resource string `json:"resource" jsonschema:"resource to list: a plural name (pods), singular (pod), short name (po), Kind (Pod), or group-qualified (deployments.apps)"`
}

// ResourceListOut is resource_list's result.
type ResourceListOut struct {
	ListMeta
	Resource string          `json:"resource" jsonschema:"the fully qualified resource that was listed"`
	Items    []ObjectSummary `json:"items"`
}

// deniedReads are resources this server will not return at any tier.
//
// The write side already refuses Secrets as "credentials"; the read side
// needs the same answer for a stronger reason. A Secret's contents in a
// model's context window is the worst read this server can perform, and it
// cannot be taken back once it is there.
//
// This does not rely on RBAC, deliberately. The on-node fallback is
// admin.conf, which is cluster-admin, so a default `stratum mcp` has the
// rights to read every Secret in the cluster. The docs recommend a narrow
// ServiceAccount for exactly that reason, but the default has to be safe
// on its own rather than only when configured well.
var deniedReads = map[schema.GroupResource]string{
	{Group: "", Resource: "secrets"}: "a Secret's contents cannot be taken back out of a model's context",
}

// describeRead names a resource the way the write side's describe does.
func describeRead(gr schema.GroupResource) string {
	if gr.Group == "" {
		return gr.Resource
	}
	return gr.Resource + "." + gr.Group
}

// checkReadable reports why reading gvr is refused, or nil.
func checkReadable(gvr schema.GroupVersionResource) error {
	gr := schema.GroupResource{Group: gvr.Group, Resource: gvr.Resource}
	if reason, denied := deniedReads[gr]; denied {
		return fmt.Errorf("%s is not readable through this server (%s). "+
			"This is a fixed policy, not a permission that can be granted by a flag: "+
			"read it with kubectl if you need it", gr.Resource, reason)
	}
	return nil
}

// DeniedReadResources lists the denied resources for the test that pins them.
func DeniedReadResources() []string {
	out := make([]string, 0, len(deniedReads))
	for gr := range deniedReads {
		out = append(out, describeRead(gr))
	}
	sort.Strings(out)
	return out
}

// ResourceListHandler lists any resource the cluster serves, including CRDs.
func ResourceListHandler(conn *kube.Connector) sdk.ToolHandlerFor[ResourceListIn, ResourceListOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in ResourceListIn) (*sdk.CallToolResult, ResourceListOut, error) {
		cl, err := cluster(conn, in.Context)
		if err != nil {
			return nil, ResourceListOut{}, err
		}
		gvr, namespaced, err := cl.ResolveResource(ctx, in.Resource)
		if err != nil {
			return nil, ResourceListOut{}, err
		}
		if err := checkReadable(gvr); err != nil {
			return nil, ResourceListOut{}, err
		}

		ri := cl.Dynamic.Resource(gvr)
		var lister dynamic.ResourceInterface = ri
		if namespaced {
			lister = ri.Namespace(in.namespace())
		}

		list, err := lister.List(ctx, in.meta())
		if err != nil {
			return nil, ResourceListOut{}, listError(err, in.Resource)
		}

		out := ResourceListOut{
			Resource: gvr.String(),
			ListMeta: newListMeta(len(list.Items), in.limit(), list.GetContinue()),
			Items:    make([]ObjectSummary, 0, len(list.Items)),
		}
		for i := range list.Items {
			out.Items = append(out.Items, summarize(&list.Items[i]))
		}
		return nil, out, nil
	}
}

// listError adds the advice a size failure needs. Without this the caller
// sees a transport error and has no idea a narrower query would succeed.
func listError(err error, what string) error {
	if kube.IsResponseTooLarge(err) {
		return fmt.Errorf("listing %s returned more data than the response limit allows; "+
			"narrow it with a namespace, a label_selector, or a smaller limit: %w", what, err)
	}
	return err
}

// ResourceGetIn is resource_get's input.
type ResourceGetIn struct {
	ObjectRef
	Resource string `json:"resource" jsonschema:"resource to read: a plural name (pods), singular (pod), short name (po), Kind (Pod), or group-qualified (deployments.apps)"`
}

// ResourceGetOut is resource_get's result.
type ResourceGetOut struct {
	Resource string         `json:"resource" jsonschema:"the fully qualified resource that was read"`
	Object   map[string]any `json:"object" jsonschema:"the object as the API server returned it"`
}

// ResourceGetHandler reads one object in full.
func ResourceGetHandler(conn *kube.Connector) sdk.ToolHandlerFor[ResourceGetIn, ResourceGetOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in ResourceGetIn) (*sdk.CallToolResult, ResourceGetOut, error) {
		if in.Name == "" {
			return nil, ResourceGetOut{}, fmt.Errorf("name is required")
		}
		cl, err := cluster(conn, in.Context)
		if err != nil {
			return nil, ResourceGetOut{}, err
		}
		gvr, namespaced, err := cl.ResolveResource(ctx, in.Resource)
		if err != nil {
			return nil, ResourceGetOut{}, err
		}
		if err := checkReadable(gvr); err != nil {
			return nil, ResourceGetOut{}, err
		}

		ri := cl.Dynamic.Resource(gvr)
		var getter dynamic.ResourceInterface = ri
		if namespaced {
			ns := in.Namespace
			if ns == "" {
				ns = metav1.NamespaceDefault
			}
			getter = ri.Namespace(ns)
		}

		obj, err := getter.Get(ctx, in.Name, metav1.GetOptions{})
		if err != nil {
			return nil, ResourceGetOut{}, err
		}
		return nil, ResourceGetOut{Resource: gvr.String(), Object: obj.Object}, nil
	}
}

// -------------------------------------------------------------------- pods

// PodSummary is one pod as an operator reads it in `kubectl get pods`.
type PodSummary struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Phase     string `json:"phase" jsonschema:"Pending, Running, Succeeded, Failed or Unknown"`
	Ready     string `json:"ready" jsonschema:"ready containers over total, e.g. 1/2"`
	Restarts  int32  `json:"restarts"`
	Node      string `json:"node,omitempty"`
	PodIP     string `json:"podIP,omitempty"`
	Age       string `json:"age,omitempty"`
	Reason    string `json:"reason,omitempty" jsonschema:"why a pod is not running, when the API server says"`
}

// PodListIn is pod_list's input.
type PodListIn struct {
	ListOptions
}

// PodListOut is pod_list's result.
type PodListOut struct {
	ListMeta
	Items []PodSummary `json:"items"`
}

// PodListHandler lists pods.
func PodListHandler(conn *kube.Connector) sdk.ToolHandlerFor[PodListIn, PodListOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in PodListIn) (*sdk.CallToolResult, PodListOut, error) {
		cl, err := cluster(conn, in.Context)
		if err != nil {
			return nil, PodListOut{}, err
		}
		list, err := cl.Typed.CoreV1().Pods(in.namespace()).List(ctx, in.meta())
		if err != nil {
			return nil, PodListOut{}, listError(err, "pods")
		}

		out := PodListOut{
			ListMeta: newListMeta(len(list.Items), in.limit(), list.Continue),
			Items:    make([]PodSummary, 0, len(list.Items)),
		}
		for i := range list.Items {
			out.Items = append(out.Items, summarizePod(&list.Items[i]))
		}
		return nil, out, nil
	}
}

func summarizePod(p *corev1.Pod) PodSummary {
	s := PodSummary{
		Name:      p.Name,
		Namespace: p.Namespace,
		Phase:     string(p.Status.Phase),
		Node:      p.Spec.NodeName,
		PodIP:     p.Status.PodIP,
		Reason:    p.Status.Reason,
	}
	if !p.CreationTimestamp.IsZero() {
		s.Age = age(p.CreationTimestamp.Time)
	}

	var ready int
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Ready {
			ready++
		}
		s.Restarts += cs.RestartCount
		// A waiting container's reason (CrashLoopBackOff, ImagePullBackOff)
		// is the single most useful field on a broken pod, and the pod-level
		// Reason is usually empty when it is set.
		if s.Reason == "" && cs.State.Waiting != nil {
			s.Reason = cs.State.Waiting.Reason
		}
	}
	s.Ready = fmt.Sprintf("%d/%d", ready, len(p.Spec.Containers))
	return s
}

// PodGetIn is pod_get's input.
type PodGetIn struct {
	ObjectRef
}

// PodGetOut is pod_get's result.
type PodGetOut struct {
	Summary    PodSummary     `json:"summary" jsonschema:"the same columns pod_list reports"`
	Containers []ContainerRow `json:"containers"`
	Conditions []ConditionRow `json:"conditions"`
	Object     map[string]any `json:"object,omitempty" jsonschema:"omitted; call resource_get with resource=pods for the raw object"`
}

// ContainerRow is one container's state.
type ContainerRow struct {
	Name     string `json:"name"`
	Image    string `json:"image"`
	Ready    bool   `json:"ready"`
	Restarts int32  `json:"restarts"`
	State    string `json:"state" jsonschema:"running, waiting or terminated"`
	Reason   string `json:"reason,omitempty"`
	Message  string `json:"message,omitempty"`
}

// ConditionRow is one pod condition.
type ConditionRow struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// PodGetHandler reads one pod, resolved into the fields a diagnosis needs.
func PodGetHandler(conn *kube.Connector) sdk.ToolHandlerFor[PodGetIn, PodGetOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in PodGetIn) (*sdk.CallToolResult, PodGetOut, error) {
		if in.Name == "" {
			return nil, PodGetOut{}, fmt.Errorf("name is required")
		}
		cl, err := cluster(conn, in.Context)
		if err != nil {
			return nil, PodGetOut{}, err
		}
		ns := in.Namespace
		if ns == "" {
			ns = metav1.NamespaceDefault
		}
		p, err := cl.Typed.CoreV1().Pods(ns).Get(ctx, in.Name, metav1.GetOptions{})
		if err != nil {
			return nil, PodGetOut{}, err
		}

		out := PodGetOut{Summary: summarizePod(p)}
		for _, cs := range p.Status.ContainerStatuses {
			row := ContainerRow{Name: cs.Name, Image: cs.Image, Ready: cs.Ready, Restarts: cs.RestartCount}
			switch {
			case cs.State.Running != nil:
				row.State = "running"
			case cs.State.Waiting != nil:
				row.State = "waiting"
				row.Reason, row.Message = cs.State.Waiting.Reason, cs.State.Waiting.Message
			case cs.State.Terminated != nil:
				row.State = "terminated"
				row.Reason, row.Message = cs.State.Terminated.Reason, cs.State.Terminated.Message
			}
			out.Containers = append(out.Containers, row)
		}
		for _, c := range p.Status.Conditions {
			out.Conditions = append(out.Conditions, ConditionRow{
				Type: string(c.Type), Status: string(c.Status), Reason: c.Reason, Message: c.Message,
			})
		}
		return nil, out, nil
	}
}

// PodLogsIn is pod_logs' input.
type PodLogsIn struct {
	ObjectRef
	Container    string `json:"container,omitempty" jsonschema:"container to read; required only when the pod has more than one"`
	TailLines    int64  `json:"tail_lines,omitempty" jsonschema:"lines from the end of the log; defaults to 200"`
	Previous     bool   `json:"previous,omitempty" jsonschema:"read the previous terminated container instead of the running one"`
	SinceSeconds int64  `json:"since_seconds,omitempty" jsonschema:"only lines newer than this many seconds"`
}

// PodLogsOut is pod_logs' result.
type PodLogsOut struct {
	Container string `json:"container" jsonschema:"the container actually read"`
	Logs      string `json:"logs"`
	Lines     int    `json:"lines"`
	Truncated bool   `json:"truncated" jsonschema:"true when the log was longer than the byte cap and the START was dropped"`
	Note      string `json:"note,omitempty"`
}

// PodLogsHandler reads a container's logs, bounded.
func PodLogsHandler(conn *kube.Connector) sdk.ToolHandlerFor[PodLogsIn, PodLogsOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in PodLogsIn) (*sdk.CallToolResult, PodLogsOut, error) {
		if in.Name == "" {
			return nil, PodLogsOut{}, fmt.Errorf("name is required")
		}
		cl, err := cluster(conn, in.Context)
		if err != nil {
			return nil, PodLogsOut{}, err
		}
		ns := in.Namespace
		if ns == "" {
			ns = metav1.NamespaceDefault
		}

		// With no container named the API server reads the only one, so the
		// result has to say which that was -- the output field is documented
		// as "the container actually read", and echoing the empty input back
		// makes it useless on exactly the single-container pods where the
		// argument is most often omitted.
		container := in.Container
		if container == "" {
			pod, perr := cl.Typed.CoreV1().Pods(ns).Get(ctx, in.Name, metav1.GetOptions{})
			if perr != nil {
				return nil, PodLogsOut{}, perr
			}
			if len(pod.Spec.Containers) == 1 {
				container = pod.Spec.Containers[0].Name
			}
		}

		tail := in.TailLines
		if tail <= 0 {
			tail = DefaultLogLines
		}
		opts := &corev1.PodLogOptions{
			Container: in.Container,
			TailLines: &tail,
			Previous:  in.Previous,
		}
		if in.SinceSeconds > 0 {
			opts.SinceSeconds = &in.SinceSeconds
		}

		stream, err := cl.Typed.CoreV1().Pods(ns).GetLogs(in.Name, opts).Stream(ctx)
		if err != nil {
			return nil, PodLogsOut{}, err
		}
		defer stream.Close()

		out, err := readLogTail(stream)
		if err != nil {
			return nil, PodLogsOut{}, err
		}
		out.Container = container
		return nil, out, nil
	}
}

// readLogTail reads a log stream and returns its last MaxLogBytes. A
// container log streams oldest-first, so the tail is what Truncated's
// schema promises the caller.
func readLogTail(r io.Reader) (PodLogsOut, error) {
	// One byte past the cap, so exactly MaxLogBytes is not truncated.
	const keep = MaxLogBytes + 1
	buf := make([]byte, 0, keep)
	chunk := make([]byte, 32<<10)
	for {
		n, rerr := r.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			if len(buf) > keep {
				buf = buf[len(buf)-keep:]
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return PodLogsOut{}, fmt.Errorf("read logs: %w", rerr)
		}
	}
	return trimLog(buf), nil
}

// trimLog caps a log at MaxLogBytes, keeping the END.
func trimLog(buf []byte) PodLogsOut {
	var out PodLogsOut
	truncated := false
	if len(buf) > MaxLogBytes {
		buf = buf[len(buf)-MaxLogBytes:]
		if nl := bytes.IndexByte(buf, '\n'); nl >= 0 {
			buf = buf[nl+1:]
		}
		truncated = true
		out.Truncated = true
		out.Note = fmt.Sprintf("Log exceeded %d bytes; the oldest lines were dropped. "+
			"Use tail_lines or since_seconds to narrow it.", MaxLogBytes)
	}
	out.Logs = string(buf)
	if out.Logs != "" {
		out.Lines = strings.Count(strings.TrimRight(out.Logs, "\n"), "\n") + 1
		return out
	}

	// Truncated-to-nothing is not the same as no output at all.
	if truncated {
		out.Note += " Nothing survived the cap: the remaining lines were longer than it."
		return out
	}

	// An empty log is ambiguous: a container that has not started and one that
	// simply has not written anything are indistinguishable from "".
	out.Note = "The container produced no output. It may not have started yet — " +
		"check pod_get for its state, or pass previous=true if it has already crashed."
	return out
}

// ------------------------------------------------------------------ events

// EventRow is one event.
type EventRow struct {
	Namespace string `json:"namespace,omitempty"`
	Type      string `json:"type" jsonschema:"Normal or Warning"`
	Reason    string `json:"reason"`
	Object    string `json:"object" jsonschema:"the involved object, as Kind/name"`
	Message   string `json:"message"`
	Count     int32  `json:"count"`
	Last      string `json:"last,omitempty" jsonschema:"when it last occurred"`
}

// EventListIn is event_list's input.
type EventListIn struct {
	ListOptions
	WarningsOnly bool `json:"warnings_only,omitempty" jsonschema:"return only Warning events, which is usually what a diagnosis wants"`
}

// EventListOut is event_list's result.
type EventListOut struct {
	ListMeta
	Items []EventRow `json:"items"`
}

// EventListHandler lists events, most recent first.
func EventListHandler(conn *kube.Connector) sdk.ToolHandlerFor[EventListIn, EventListOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in EventListIn) (*sdk.CallToolResult, EventListOut, error) {
		cl, err := cluster(conn, in.Context)
		if err != nil {
			return nil, EventListOut{}, err
		}

		opts := in.meta()
		if in.WarningsOnly {
			// Composed rather than replacing whatever the caller supplied.
			if opts.FieldSelector == "" {
				opts.FieldSelector = "type=Warning"
			} else {
				opts.FieldSelector += ",type=Warning"
			}
		}

		list, err := cl.Typed.CoreV1().Events(in.namespace()).List(ctx, opts)
		if err != nil {
			return nil, EventListOut{}, listError(err, "events")
		}

		out := EventListOut{
			ListMeta: newListMeta(len(list.Items), in.limit(), list.Continue),
			Items:    make([]EventRow, 0, len(list.Items)),
		}
		for i := range list.Items {
			e := &list.Items[i]
			row := EventRow{
				Namespace: e.Namespace,
				Type:      e.Type,
				Reason:    e.Reason,
				Object:    fmt.Sprintf("%s/%s", e.InvolvedObject.Kind, e.InvolvedObject.Name),
				Message:   e.Message,
				Count:     e.Count,
			}
			if ts := eventTime(e); !ts.IsZero() {
				row.Last = ts.UTC().Format(time.RFC3339)
			}
			out.Items = append(out.Items, row)
		}

		// The API server returns events in an unhelpful order, and a model reading
		// the first ten of an unsorted list draws conclusions from whichever ten it
		// got.
		sort.SliceStable(out.Items, func(i, j int) bool { return out.Items[i].Last > out.Items[j].Last })
		// The apiserver has no server-side sort, so this orders the page,
		// not the cluster.
		if out.Truncated {
			out.Note += " These are sorted newest-first WITHIN this page only; the newest events " +
				"in the cluster may not be in it. Narrow with namespace or field_selector rather than paging."
		}
		return nil, out, nil
	}
}

// eventTime picks the most meaningful timestamp an event carries. LastTimestamp
// is empty on events from the newer events API, where EventTime carries it.
func eventTime(e *corev1.Event) time.Time {
	switch {
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	default:
		return e.CreationTimestamp.Time
	}
}

// -------------------------------------------------------- namespaces, nodes

// NamespaceRow is one namespace.
type NamespaceRow struct {
	Name   string `json:"name"`
	Status string `json:"status" jsonschema:"Active or Terminating"`
	Age    string `json:"age,omitempty"`
}

// NamespaceListIn is namespace_list's input.
type NamespaceListIn struct {
	Context       string `json:"context,omitempty" jsonschema:"kubeconfig context to address; omit for the current one"`
	LabelSelector string `json:"label_selector,omitempty" jsonschema:"label selector"`
	Limit         int    `json:"limit,omitempty" jsonschema:"maximum items to return; defaults to 100, capped at 500"`
	ContinueToken string `json:"continue_token,omitempty" jsonschema:"token from a previous truncated response"`
}

// NamespaceListOut is namespace_list's result.
type NamespaceListOut struct {
	ListMeta
	Items []NamespaceRow `json:"items"`
}

// NamespaceListHandler lists namespaces.
func NamespaceListHandler(conn *kube.Connector) sdk.ToolHandlerFor[NamespaceListIn, NamespaceListOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in NamespaceListIn) (*sdk.CallToolResult, NamespaceListOut, error) {
		cl, err := cluster(conn, in.Context)
		if err != nil {
			return nil, NamespaceListOut{}, err
		}
		limit := ListOptions{Limit: in.Limit}.limit()
		list, err := cl.Typed.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
			LabelSelector: in.LabelSelector,
			Limit:         int64(limit),
			Continue:      in.ContinueToken,
		})
		if err != nil {
			return nil, NamespaceListOut{}, listError(err, "namespaces")
		}

		out := NamespaceListOut{
			ListMeta: newListMeta(len(list.Items), limit, list.Continue),
			Items:    make([]NamespaceRow, 0, len(list.Items)),
		}
		for i := range list.Items {
			n := &list.Items[i]
			row := NamespaceRow{Name: n.Name, Status: string(n.Status.Phase)}
			if !n.CreationTimestamp.IsZero() {
				row.Age = age(n.CreationTimestamp.Time)
			}
			out.Items = append(out.Items, row)
		}
		return nil, out, nil
	}
}

// NodeRow is one node as `kubectl get nodes` shows it.
type NodeRow struct {
	Name             string   `json:"name"`
	Ready            string   `json:"ready" jsonschema:"True, False or Unknown, from the Ready condition"`
	Roles            []string `json:"roles,omitempty"`
	Schedulable      bool     `json:"schedulable"`
	KubeletVersion   string   `json:"kubeletVersion,omitempty"`
	OSImage          string   `json:"osImage,omitempty"`
	ContainerRuntime string   `json:"containerRuntime,omitempty"`
	InternalIP       string   `json:"internalIP,omitempty"`
	Age              string   `json:"age,omitempty"`
}

// NodeListIn is node_list's input.
type NodeListIn struct {
	Context       string `json:"context,omitempty" jsonschema:"kubeconfig context to address; omit for the current one"`
	LabelSelector string `json:"label_selector,omitempty" jsonschema:"label selector"`
	Limit         int    `json:"limit,omitempty" jsonschema:"maximum items to return; defaults to 100, capped at 500"`
	ContinueToken string `json:"continue_token,omitempty" jsonschema:"token from a previous truncated response"`
}

// NodeListOut is node_list's result.
type NodeListOut struct {
	ListMeta
	Items []NodeRow `json:"items"`
}

// NodeListHandler lists nodes.
func NodeListHandler(conn *kube.Connector) sdk.ToolHandlerFor[NodeListIn, NodeListOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in NodeListIn) (*sdk.CallToolResult, NodeListOut, error) {
		cl, err := cluster(conn, in.Context)
		if err != nil {
			return nil, NodeListOut{}, err
		}
		limit := ListOptions{Limit: in.Limit}.limit()
		list, err := cl.Typed.CoreV1().Nodes().List(ctx, metav1.ListOptions{
			LabelSelector: in.LabelSelector,
			Limit:         int64(limit),
			Continue:      in.ContinueToken,
		})
		if err != nil {
			return nil, NodeListOut{}, listError(err, "nodes")
		}

		out := NodeListOut{
			ListMeta: newListMeta(len(list.Items), limit, list.Continue),
			Items:    make([]NodeRow, 0, len(list.Items)),
		}
		for i := range list.Items {
			out.Items = append(out.Items, summarizeNode(&list.Items[i]))
		}
		return nil, out, nil
	}
}

func summarizeNode(n *corev1.Node) NodeRow {
	row := NodeRow{
		Name:             n.Name,
		Ready:            "Unknown",
		Schedulable:      !n.Spec.Unschedulable,
		KubeletVersion:   n.Status.NodeInfo.KubeletVersion,
		OSImage:          n.Status.NodeInfo.OSImage,
		ContainerRuntime: n.Status.NodeInfo.ContainerRuntimeVersion,
	}
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			row.Ready = string(c.Status)
		}
	}
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			row.InternalIP = a.Address
		}
	}
	// Roles are encoded as labels, which is a convention rather than an API,
	// so both spellings in use are read.
	for k := range n.Labels {
		if role, ok := strings.CutPrefix(k, "node-role.kubernetes.io/"); ok && role != "" {
			row.Roles = append(row.Roles, role)
		}
	}
	sort.Strings(row.Roles)
	if !n.CreationTimestamp.IsZero() {
		row.Age = age(n.CreationTimestamp.Time)
	}
	return row
}

// ---------------------------------------------------------------- contexts

// ContextListIn is context_list's (empty) input.
type ContextListIn struct{}

// ContextListOut is context_list's result.
type ContextListOut struct {
	Items  []kube.ContextInfo `json:"items"`
	Source string             `json:"source" jsonschema:"where the configuration came from: explicit, KUBECONFIG, home, fallback or in-cluster"`
	Note   string             `json:"note,omitempty"`
}

// ContextListHandler lists the clusters this server can address.
func ContextListHandler(conn *kube.Connector) sdk.ToolHandlerFor[ContextListIn, ContextListOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, _ ContextListIn) (*sdk.CallToolResult, ContextListOut, error) {
		items, err := conn.Contexts()
		if err != nil {
			return nil, ContextListOut{}, err
		}
		out := ContextListOut{Items: items}

		// Reporting an empty list with no explanation reads as a failure.
		if len(items) == 0 {
			out.Source = string(kube.SourceInCluster)
			out.Note = "No kubeconfig contexts: this server uses in-cluster credentials and addresses one cluster. Omit the context argument."
			return nil, out, nil
		}
		if cl, err := conn.Cluster(""); err == nil {
			out.Source = string(cl.Source)
		}
		return nil, out, nil
	}
}
