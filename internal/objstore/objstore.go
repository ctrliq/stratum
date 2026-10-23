// Package objstore is a thin S3 client (AWS SDK for Go v2) for offloading etcd
// snapshots to an S3-compatible object store — AWS S3, MinIO, Ceph RGW, etc. A
// custom BaseEndpoint + path-style addressing makes the non-AWS stores work.
// Credentials are passed in a Target (loaded from /etc/stratum/snapshot.conf or the
// environment) — never on the command line.
package objstore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
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

// Prune deletes objects whose base name owned accepts, keeping the newest
// `keep` (names carry a sortable UTC timestamp, so lexical == chronological).
// keep <= 0 is a no-op (never prune-all).
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
	cli, err := t.client(ctx)
	if err != nil {
		return err
	}
	for _, o := range matches[:len(matches)-keep] {
		if _, err := cli.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(t.Bucket), Key: aws.String(t.Key(o.Name)),
		}); err != nil {
			return fmt.Errorf("objstore: prune %s: %w", o.Name, err)
		}
	}
	return nil
}
