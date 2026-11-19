package upgrade

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/nodejob"
)

// PostCheckConfigMap is the kube-system ConfigMap that declares the checks a
// cluster upgrade runs on each node after its upgrade and before it uncordons
// it. Each data key names one check.
const PostCheckConfigMap = "stratum-upgrade-node-checks"

const (
	defaultPostCheckTimeout = 15 * time.Minute
	maxPostCheckTimeout     = 24 * time.Hour
)

// postCheck is one entry of PostCheckConfigMap.
type postCheck struct {
	Name           string   `json:"-"`
	NodeSelector   string   `json:"nodeSelector"`
	Command        []string `json:"command"`
	TimeoutSeconds int      `json:"timeoutSeconds,omitempty"`

	selector labels.Selector
}

func (c postCheck) timeout() time.Duration {
	if c.TimeoutSeconds == 0 {
		return defaultPostCheckTimeout
	}
	return time.Duration(c.TimeoutSeconds) * time.Second
}

// errPostCheckFailed marks a check that ran on the node and did not pass.
var errPostCheckFailed = errors.New("post-upgrade node check failed")

// runPostUpgradeCheck is nodejob.Run for a post-upgrade check Job; a var so a
// test can answer for a node.
var runPostUpgradeCheck = nodejob.Run

// loadPostChecks reads PostCheckConfigMap. A cluster without it has no checks;
// one that cannot be read or parsed refuses the run before any node is touched.
func loadPostChecks(ctx context.Context, cs kubernetes.Interface) ([]postCheck, error) {
	var cm *corev1.ConfigMap
	err := retryAPI(ctx, func(ctx context.Context) error {
		got, err := cs.CoreV1().ConfigMaps(cuNamespace).Get(ctx, PostCheckConfigMap, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			cm = nil
			return nil
		}
		cm = got
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("read %s/%s: %w", cuNamespace, PostCheckConfigMap, err)
	}
	if cm == nil {
		return nil, nil
	}
	checks, err := parsePostChecks(cm)
	if err != nil {
		return nil, fmt.Errorf("%s/%s: %w", cuNamespace, PostCheckConfigMap, err)
	}
	return checks, nil
}

func parsePostChecks(cm *corev1.ConfigMap) ([]postCheck, error) {
	if len(cm.BinaryData) > 0 {
		return nil, errors.New("binaryData is set; each check is a YAML document under data")
	}
	names := make([]string, 0, len(cm.Data))
	for k := range cm.Data {
		names = append(names, k)
	}
	sort.Strings(names)
	checks := make([]postCheck, 0, len(names))
	for _, name := range names {
		var c postCheck
		if err := yaml.UnmarshalStrict([]byte(cm.Data[name]), &c); err != nil {
			return nil, fmt.Errorf("check %q: %w", name, err)
		}
		c.Name = name
		if err := c.validate(); err != nil {
			return nil, fmt.Errorf("check %q: %w", name, err)
		}
		checks = append(checks, c)
	}
	return checks, nil
}

func (c *postCheck) validate() error {
	if strings.TrimSpace(c.NodeSelector) == "" {
		return errors.New("nodeSelector is required")
	}
	sel, err := labels.Parse(c.NodeSelector)
	if err != nil {
		return fmt.Errorf("nodeSelector: %w", err)
	}
	c.selector = sel
	if len(c.Command) == 0 {
		return errors.New("command is required")
	}
	if !path.IsAbs(c.Command[0]) {
		return fmt.Errorf("command: %q is not an absolute path to a binary on the node", c.Command[0])
	}
	if c.TimeoutSeconds < 0 || time.Duration(c.TimeoutSeconds)*time.Second > maxPostCheckTimeout {
		return fmt.Errorf("timeoutSeconds %d is out of range 1-%d (0 or unset is %d)",
			c.TimeoutSeconds, int(maxPostCheckTimeout.Seconds()), int(defaultPostCheckTimeout.Seconds()))
	}
	return nil
}

// printPostChecks is the plan's line for each declared check.
func printPostChecks(w io.Writer, checks []postCheck) {
	for _, c := range checks {
		fmt.Fprintf(w, "  after each node's upgrade, before its uncordon: check %s on nodes matching %s: %s\n",
			c.Name, c.NodeSelector, strings.Join(c.Command, " "))
	}
}

// postCheckArgv runs c's command on the host, as the upgrade Job runs the
// staged binary.
func postCheckArgv(c postCheck) []string {
	return append([]string{"nsenter", "-t", "1", "-m", "-u", "-i", "-n", "-p", "--"}, c.Command...)
}

// runPostChecks runs, one at a time, every check whose selector matches the
// node's labels as they are now. A check that does not pass returns an error
// wrapping errPostCheckFailed.
func runPostChecks(ctx context.Context, w io.Writer, cs kubernetes.Interface, n Node, job string, o ClusterOptions) error {
	if len(o.postChecks) == 0 {
		return nil
	}
	var node *corev1.Node
	if err := retryAPI(ctx, func(ctx context.Context) (err error) {
		node, err = cs.CoreV1().Nodes().Get(ctx, n.Name, metav1.GetOptions{})
		return err
	}); err != nil {
		return fmt.Errorf("read %s's labels for its post-upgrade checks: %w", n.Name, err)
	}
	set := labels.Set(node.Labels)
	for _, c := range o.postChecks {
		if !c.selector.Matches(set) {
			continue
		}
		if err := o.leaseErr(); err != nil {
			return err
		}
		k8sutil.Step(w, fmt.Sprintf("Post-upgrade check %s on %s", c.Name, n.Name))
		deadline := c.timeout()
		if err := runPostUpgradeCheck(ctx, cs, nodejob.Spec{
			Output:         w,
			Namespace:      cuNamespace,
			Name:           job,
			Node:           n.Name,
			LabelKey:       upgradeJobLabel,
			Container:      "check",
			Argv:           postCheckArgv(c),
			ActiveDeadline: deadline,
			PollAttempts:   int(deadline / (5 * time.Second)),
			Kind:           "post-upgrade check " + c.Name,
			Critical:       true,
		}); err != nil {
			return fmt.Errorf("%w: %s on %s: %w", errPostCheckFailed, c.Name, n.Name, err)
		}
		fmt.Fprintf(w, "  %s passed check %s\n", n.Name, c.Name)
	}
	return nil
}

// holdFailedNode keeps a node that failed a post-upgrade check out of
// scheduling: a control plane's own upgrade Job has already uncordoned it, so
// it is cordoned again under the upgrade's claim, which the next run's
// successful check undoes. The node stays unstamped, so the next run takes the
// full path and runs the checks again.
func holdFailedNode(ctx context.Context, cs kubernetes.Interface, n Node, job string, o ClusterOptions, cause error) error {
	from := shellQuote(o.From)
	held := "it is left cordoned"
	if err := cordonForUpgrade(ctx, cs, n.Name, o.Target, o.lease); err != nil {
		held = fmt.Sprintf("cordoning it again failed (%v), so it may be schedulable: run %s kubectl cordon %s", err, from, n.Name)
	}
	return fmt.Errorf("%w. %s runs %s, but it is not recorded as upgraded, and %s. "+
		"Read why: %s kubectl -n %s logs job/%s. Once the node is fixed, re-run %s upgrade --cluster --target %s --from %s --yes, "+
		"which runs the check again before it uncordons the node",
		cause, n.Name, o.Target, held, from, cuNamespace, job, from, o.Target, from)
}
