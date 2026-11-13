// Package objstore is a thin S3 client (AWS SDK for Go v2) for offloading etcd
// snapshots to an S3-compatible object store — AWS S3, MinIO, Ceph RGW, etc. A
// custom BaseEndpoint + path-style addressing makes the non-AWS stores work.
// Credentials are passed in a Target (loaded from /etc/stratum/snapshot.conf or the
// environment) — never on the command line.
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

// Target describes an S3-compatible destination for snapshots.
type Target struct {
	Endpoint  string // custom endpoint for S3-compatible stores; empty = real AWS
	Region    string // default "us-east-1"
	Bucket    string
	Prefix    string // key prefix (folder within the bucket)
	AccessKey string
	SecretKey string
	PathStyle bool // true for MinIO/Ceph and most non-AWS endpoints
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

// Download fetches object `name` into root (multipart), as a file called name.
// The file is written 0600 to a temp file beside it and renamed into place once
// complete, so a failed download leaves an existing file untouched, and a
// symlink at that name is replaced, never followed. root is a directory handle,
// not a path, so the directory that was checked is the one written even if its
// name is swapped meanwhile.
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

// tempName is the name of a temp file writeAtomic makes for name:
// ".<name>.<uint32 in decimal>".
func tempName(name string) string {
	return "." + name + "." + strconv.FormatUint(uint64(rand.Uint32()), 10)
}

// isTempName reports whether file is a temp file writeAtomic could have made for name.
func isTempName(file, name string) bool {
	digits, ok := strings.CutPrefix(file, "."+name+".")
	if !ok {
		return false
	}
	// Only what tempName generates: a canonical uint32, so no overflow or leading zeros.
	n, err := strconv.ParseUint(digits, 10, 32)
	return err == nil && strconv.FormatUint(n, 10) == digits
}

// writeAtomic runs fill on a new 0600 file in root (O_EXCL, so never a
// pre-planted path), syncs it and renames it to name. The temp file is removed
// on any error.
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

// RemoveStaleTemps removes from root the regular files named like the temp
// files Download makes for name (".<name>.<digits>") that were last written more
// than olderThan ago: what a SIGKILL or power loss mid-download leaves, a
// partial copy of a snapshot. Younger ones may be a concurrent download's.
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

// Object is one item in the store.
type Object struct {
	Name     string // base name (prefix stripped)
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
				Name:     path.Base(aws.ToString(o.Key)),
				Size:     aws.ToInt64(o.Size),
				Modified: aws.ToTime(o.LastModified),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ListEnd classifies how a bounded listing ended. Only ListEndedComplete
// says the objects are the store's exact total; every other end stopped the
// listing before the end, so the objects returned are a floor and the
// store's total is unknown — never zero.
type ListEnd string

const (
	// ListEndedComplete: every page was listed to the end.
	ListEndedComplete ListEnd = "complete"
	// ListEndedCapped: a page or byte cap stopped the listing before the end.
	ListEndedCapped ListEnd = "capped"
	// ListEndedDenied: the store refused the listing (403) — access denied
	// on the bucket, a wrong key, a policy — distinct from an ordinary
	// failure because it is the store's answer, not a broken request.
	ListEndedDenied ListEnd = "denied"
	// ListEndedTimeout: the listing's time bound ran out mid-listing.
	ListEndedTimeout ListEnd = "timeout"
	// ListEndedCancelled: the caller cancelled the listing mid-listing.
	ListEndedCancelled ListEnd = "cancelled"
	// ListEndedFailed: the listing failed any other way.
	ListEndedFailed ListEnd = "failed"
)

// The built-in bounds of a bounded listing: a caller that passes no cap gets
// these, never an unbounded listing.
const (
	// ListKeysPerPage is the keys the S3 ListObjectsV2 API serves one page:
	// the most a single page carries, and the grain every page cap below
	// is counted in.
	ListKeysPerPage = 1000
	// DefaultListMaxPages is the page cap. At ListKeysPerPage keys a page
	// it bounds a listing at a million objects.
	DefaultListMaxPages = 1000
	// DefaultListMaxBytes is the byte cap: how many object bytes one
	// listing may account for.
	DefaultListMaxBytes = int64(1) << 40
	// DefaultListMaxTime is the time cap: how long one listing may take,
	// however patient the caller's context is.
	DefaultListMaxTime = 30 * time.Second
)

// ListOptions carries the finite bounds of a bounded listing. A zero or
// negative cap means the built-in default, never an unbounded listing.
type ListOptions struct {
	// MaxPages caps how many pages the listing may request (the page cap).
	MaxPages int
	// MaxBytes caps how many object bytes the listing may account for.
	MaxBytes int64
	// MaxTime caps how long the listing may take, on top of whatever
	// deadline the caller's context already carries (the shorter bound
	// wins).
	MaxTime time.Duration
}

// ListResult is a bounded listing's objects and how the listing ended. The
// end classification is safe by construction: it names the condition —
// denied, timeout, cancellation, a cap — and never carries the store's raw
// error text.
type ListResult struct {
	// Objects are the objects listed, sorted by name. They are the store's
	// exact total only when End is ListEndedComplete; otherwise they are a
	// floor.
	Objects []Object
	// End says how the listing ended.
	End ListEnd
}

// ListBounded lists the objects under the target's prefix under finite page,
// byte and time bounds, ending complete when it reached the end and
// classified — denied, timeout, cancelled, capped, failed — when it stopped
// short. The caller's context is propagated into the S3 calls, so a hung or
// denied store returns when the bound or the caller ends it, not when the
// store does. A key the store lists again on a later page is one object: a
// duplicate across pages is the same object, listed once.
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
	// The time cap on top of the caller's own deadline: whichever bound is
	// shorter ends the listing, so it is finite however patient the caller
	// is — and however silent the store is.
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
			// What the pages so far listed stands as a floor; the
			// classification names what stopped the rest. The floor
			// leaves through the same exit as every other end, so it
			// keeps the sorted-by-name contract the result documents.
			end = listEnd(err)
			break list
		}
		pages++
		for _, ob := range page.Contents {
			name := path.Base(aws.ToString(ob.Key))
			if seen[name] {
				// The store listed this key again on a later page:
				// the same object, not a second one.
				continue
			}
			seen[name] = true
			size := aws.ToInt64(ob.Size)
			if accounted+size > o.MaxBytes {
				end = ListEndedCapped
				break list
			}
			accounted += size
			out = append(out, Object{
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

// listEnd classifies how a listing error ended the listing. It is safe by
// construction: it names the condition — a denial, a timeout, a cancellation,
// a failure — and never carries the store's raw error text.
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

// Prune deletes objects whose base name owned accepts, keeping the newest
// `keep` by LastModified with the name breaking ties, and tries every older
// one even when a delete fails. keep <= 0 is a no-op (never prune-all).
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
