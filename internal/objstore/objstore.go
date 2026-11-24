// Package objstore offloads etcd snapshots to S3-compatible object stores.
package objstore

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type Target struct {
	Endpoint  string
	Region    string
	Bucket    string
	Prefix    string
	AccessKey string
	SecretKey string
	PathStyle bool
}

// Enabled reports whether an S3 target is configured (a bucket is set).
func (t *Target) Enabled() bool { return t != nil && t.Bucket != "" }

func (t *Target) client(ctx context.Context) (*s3.Client, error) {
	if t.Bucket == "" {
		return nil, fmt.Errorf("objstore: bucket is required")
	}
	region := t.Region
	if region == "" {
		region = "us-east-1"
	}
	loadOpts := []func(*config.LoadOptions) error{config.WithRegion(region)}
	if t.AccessKey != "" {
		loadOpts = append(loadOpts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(t.AccessKey, t.SecretKey, "")))
	}
	cfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("objstore: load AWS config: %w", err)
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = t.PathStyle
		if t.Endpoint != "" {
			o.BaseEndpoint = aws.String(t.Endpoint)
		}
	}), nil
}

// Key returns the full object key for a snapshot base name under the prefix.
func (t *Target) Key(name string) string {
	if t.Prefix == "" {
		return name
	}
	return strings.TrimSuffix(t.Prefix, "/") + "/" + name
}

// Upload streams localPath to the target as object `name` (multipart for large files).
func (t *Target) Upload(ctx context.Context, localPath, name string) error {
	cli, err := t.client(ctx)
	if err != nil {
		return err
	}
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	up := manager.NewUploader(cli)
	if _, err := up.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(t.Bucket), Key: aws.String(t.Key(name)), Body: f,
	}); err != nil {
		return fmt.Errorf("objstore: upload %s: %w", name, err)
	}
	return nil
}

// Download writes name into root 0600, atomically; root is a handle, immune to a path-name swap.
func (t *Target) Download(ctx context.Context, root *os.Root, name string) error {
	cli, err := t.client(ctx)
	if err != nil {
		return err
	}
	dl := manager.NewDownloader(cli)
	return writeAtomic(root, name, func(f *os.File) error {
		_, err := dl.Download(ctx, f, &s3.GetObjectInput{
			Bucket: aws.String(t.Bucket), Key: aws.String(t.Key(name)),
		})
		if err != nil {
			return fmt.Errorf("objstore: download %s: %w", name, err)
		}
		return nil
	})
}

func tempName(name string) string {
	return "." + name + "." + strconv.FormatUint(uint64(rand.Uint32()), 10)
}

func isTempName(file, name string) bool {
	digits, ok := strings.CutPrefix(file, "."+name+".")
	if !ok {
		return false
	}
	// Only what tempName generates: a canonical uint32, so no overflow or leading zeros.
	n, err := strconv.ParseUint(digits, 10, 32)
	return err == nil && strconv.FormatUint(n, 10) == digits
}

// O_EXCL: a pre-planted temp name can never win.
func writeAtomic(root *os.Root, name string, fill func(*os.File) error) (err error) {
	var f *os.File
	var tmp string
	for range 10 {
		tmp = tempName(name)
		f, err = root.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if !errors.Is(err, fs.ErrExist) {
			break
		}
	}
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = root.Remove(tmp)
		}
	}()
	if err = fill(f); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return root.Rename(tmp, name)
}

// RemoveStaleTemps removes Download's stale temp files; younger ones may be a live download's.
func RemoveStaleTemps(root *os.Root, name string, olderThan time.Duration) error {
	d, err := root.Open(".")
	if err != nil {
		return err
	}
	defer d.Close()
	files, err := d.Readdirnames(-1)
	if err != nil {
		return err
	}
	var errs []error
	for _, f := range files {
		if !isTempName(f, name) {
			continue
		}
		// Lstat, not the entry's type: it is the file being removed that must be regular.
		fi, err := root.Lstat(f)
		if err != nil || !fi.Mode().IsRegular() || time.Since(fi.ModTime()) < olderThan {
			continue
		}
		if err := root.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

type Object struct {
	// Key keeps trailing slashes, for consumers separating prefix markers.
	Key      string `json:"-"`
	Name     string
	Size     int64
	Modified time.Time
}

// List returns the objects under the target's prefix, sorted by name.
func (t *Target) List(ctx context.Context) ([]Object, error) {
	cli, err := t.client(ctx)
	if err != nil {
		return nil, err
	}
	var prefix *string
	if t.Prefix != "" {
		prefix = aws.String(strings.TrimSuffix(t.Prefix, "/") + "/")
	}
	var out []Object
	p := s3.NewListObjectsV2Paginator(cli, &s3.ListObjectsV2Input{
		Bucket: aws.String(t.Bucket), Prefix: prefix, Delimiter: aws.String("/"),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("objstore: list: %w", err)
		}
		for _, o := range page.Contents {
			out = append(out, Object{
				Key:      aws.ToString(o.Key),
				Name:     path.Base(aws.ToString(o.Key)),
				Size:     aws.ToInt64(o.Size),
				Modified: aws.ToTime(o.LastModified),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ListEnd classifies how a bounded listing ended.
type ListEnd string

const (
	ListEndedComplete ListEnd = "complete"
	ListEndedCapped   ListEnd = "capped"
	// ListEndedDenied is the store's refusal, not a broken request.
	ListEndedDenied    ListEnd = "denied"
	ListEndedTimeout   ListEnd = "timeout"
	ListEndedCancelled ListEnd = "cancelled"
	ListEndedFailed    ListEnd = "failed"
)

// A caller passing no cap gets these, never an unbounded listing.
const (
	// ListKeysPerPage is the most keys one ListObjectsV2 page carries.
	ListKeysPerPage     = 1000
	DefaultListMaxPages = 1000
	DefaultListMaxBytes = int64(1) << 40
	DefaultListMaxTime  = 30 * time.Second
)

// ListOptions bounds a listing; a zero or negative cap means the default, never unbounded.
type ListOptions struct {
	MaxPages int
	MaxBytes int64
	MaxTime  time.Duration
}

// ListResult is a listing's objects and its end; End never carries the store's raw error text.
type ListResult struct {
	// Objects are the exact total only when End is ListEndedComplete; otherwise a floor.
	Objects []Object
	End     ListEnd
}

// ListBounded lists the store's objects under the target's prefix with finite bounds.
func (t *Target) ListBounded(ctx context.Context, o ListOptions) ListResult {
	if o.MaxPages <= 0 {
		o.MaxPages = DefaultListMaxPages
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultListMaxBytes
	}
	if o.MaxTime <= 0 {
		o.MaxTime = DefaultListMaxTime
	}
	ctx, cancel := context.WithTimeout(ctx, o.MaxTime)
	defer cancel()

	var res ListResult
	if t == nil || t.Bucket == "" {
		res.End = ListEndedFailed
		return res
	}
	cli, err := t.client(ctx)
	if err != nil {
		res.End = ListEndedFailed
		return res
	}
	var prefix *string
	if t.Prefix != "" {
		prefix = aws.String(strings.TrimSuffix(t.Prefix, "/") + "/")
	}

	var out []Object
	var accounted int64
	pages := 0
	end := ListEndedComplete
	seen := make(map[string]bool)
	p := s3.NewListObjectsV2Paginator(cli, &s3.ListObjectsV2Input{
		Bucket: aws.String(t.Bucket), Prefix: prefix, Delimiter: aws.String("/"),
	})
list:
	for p.HasMorePages() {
		if pages == o.MaxPages {
			end = ListEndedCapped
			break list
		}
		page, err := p.NextPage(ctx)
		if err != nil {
			end = listEnd(err)
			break list
		}
		pages++
		for _, ob := range page.Contents {
			key := aws.ToString(ob.Key)
			name := path.Base(key)
			if seen[key] {
				continue
			}
			seen[key] = true
			size := aws.ToInt64(ob.Size)
			if accounted+size > o.MaxBytes {
				end = ListEndedCapped
				break list
			}
			accounted += size
			out = append(out, Object{
				Key:      key,
				Name:     name,
				Size:     size,
				Modified: aws.ToTime(ob.LastModified),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	res.Objects = out
	res.End = end
	return res
}

// listEnd names a condition; it never carries the store's raw error text.
func listEnd(err error) ListEnd {
	switch {
	case err == nil:
		return ListEndedComplete
	case errors.Is(err, context.Canceled):
		return ListEndedCancelled
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded):
		return ListEndedTimeout
	}
	var re *awshttp.ResponseError
	if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusForbidden {
		return ListEndedDenied
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ListEndedTimeout
	}
	return ListEndedFailed
}

// Prune deletes the owned objects older than the newest keep; keep <= 0 is a no-op.
func (t *Target) Prune(ctx context.Context, owned func(name string) bool, keep int) error {
	if keep <= 0 {
		return nil
	}
	objs, err := t.List(ctx)
	if err != nil {
		return err
	}
	var matches []Object
	for _, o := range objs {
		if owned(o.Name) {
			matches = append(matches, o)
		}
	}
	if len(matches) <= keep {
		return nil
	}
	slices.SortFunc(matches, func(a, b Object) int {
		return cmp.Or(a.Modified.Compare(b.Modified), strings.Compare(a.Name, b.Name))
	})
	cli, err := t.client(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, o := range matches[:len(matches)-keep] {
		if _, err := cli.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(t.Bucket), Key: aws.String(t.Key(o.Name)),
		}); err != nil {
			errs = append(errs, fmt.Errorf("objstore: prune %s: %w", o.Name, err))
		}
	}
	return errors.Join(errs...)
}
