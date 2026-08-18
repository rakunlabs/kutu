package s3serve

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/rakunlabs/kutu/internal/rawfs/localfs"
	"github.com/rakunlabs/kutu/internal/serve/ftpserve"
	"github.com/rakunlabs/kutu/internal/service"
)

// startTestServer boots an S3 server on a free port backed by a local
// temp dir and returns a minio client pointed at it.
func startTestServer(t *testing.T) (*minio.Client, string) {
	t.Helper()

	root := t.TempDir()
	lfs, err := localfs.New(root)
	if err != nil {
		t.Fatalf("localfs: %v", err)
	}

	shares := []ftpserve.Share{
		{
			Name:    "data",
			Sources: []ftpserve.ShareSource{{Mount: "data", FS: lfs}},
		},
	}
	users := []ftpserve.User{
		{Username: "testkey", Password: "testsecret1234"},
		{Username: "rokey", Password: "rosecret1234", ReadOnly: true},
	}

	port := freePort(t)
	cfg := &service.S3ServeSettings{Enabled: true, Host: "127.0.0.1", Port: port}

	srv, err := NewServer(cfg, shares, users)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	srv.Start(ctx)
	t.Cleanup(func() {
		cancel()
		srv.Stop()
	})

	endpoint := fmt.Sprintf("127.0.0.1:%d", port)
	waitForListen(t, endpoint)

	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4("testkey", "testsecret1234", ""),
		Secure: false,
	})
	if err != nil {
		t.Fatalf("minio client: %v", err)
	}

	return client, root
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() //nolint:errcheck
	return port
}

func waitForListen(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close() //nolint:errcheck
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server did not start listening on %s", addr)
}

func TestListBuckets(t *testing.T) {
	client, _ := startTestServer(t)

	buckets, err := client.ListBuckets(context.Background())
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Name != "data" {
		t.Fatalf("expected bucket [data], got %+v", buckets)
	}

	exists, err := client.BucketExists(context.Background(), "data")
	if err != nil || !exists {
		t.Fatalf("BucketExists(data) = %v, %v", exists, err)
	}
	exists, err = client.BucketExists(context.Background(), "nope")
	if err != nil {
		t.Fatalf("BucketExists(nope): %v", err)
	}
	if exists {
		t.Fatal("bucket nope should not exist")
	}
}

func TestPutGetStatDelete(t *testing.T) {
	client, root := startTestServer(t)
	ctx := context.Background()

	content := []byte("hello s3 world")
	_, err := client.PutObject(ctx, "data", "dir/hello.txt",
		bytes.NewReader(content), int64(len(content)), minio.PutObjectOptions{ContentType: "text/plain"})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// File must land on disk.
	onDisk, err := os.ReadFile(filepath.Join(root, "dir", "hello.txt"))
	if err != nil || !bytes.Equal(onDisk, content) {
		t.Fatalf("on-disk content mismatch: %v %q", err, onDisk)
	}

	// Stat
	st, err := client.StatObject(ctx, "data", "dir/hello.txt", minio.StatObjectOptions{})
	if err != nil {
		t.Fatalf("StatObject: %v", err)
	}
	if st.Size != int64(len(content)) {
		t.Fatalf("Stat size = %d, want %d", st.Size, len(content))
	}

	// Get
	obj, err := client.GetObject(ctx, "data", "dir/hello.txt", minio.GetObjectOptions{})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	got, err := io.ReadAll(obj)
	if err != nil {
		t.Fatalf("read object: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("GetObject = %q, want %q", got, content)
	}

	// Range get
	obj, err = client.GetObject(ctx, "data", "dir/hello.txt", minio.GetObjectOptions{})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	buf := make([]byte, 5)
	if _, err := obj.ReadAt(buf, 6); err != nil && err != io.EOF {
		t.Fatalf("ReadAt: %v", err)
	}
	if string(buf) != "s3 wo" {
		t.Fatalf("range read = %q, want %q", buf, "s3 wo")
	}

	// Delete
	if err := client.RemoveObject(ctx, "data", "dir/hello.txt", minio.RemoveObjectOptions{}); err != nil {
		t.Fatalf("RemoveObject: %v", err)
	}
	if _, err := client.StatObject(ctx, "data", "dir/hello.txt", minio.StatObjectOptions{}); err == nil {
		t.Fatal("object should be gone after delete")
	}

	// Deleting a missing key succeeds (S3 semantics).
	if err := client.RemoveObject(ctx, "data", "missing.txt", minio.RemoveObjectOptions{}); err != nil {
		t.Fatalf("RemoveObject(missing): %v", err)
	}
}

func TestListObjects(t *testing.T) {
	client, root := startTestServer(t)
	ctx := context.Background()

	files := []string{
		"a.txt",
		"docs/readme.md",
		"docs/sub/deep.txt",
		"logs/app.log",
		"z.bin",
	}
	for _, f := range files {
		full := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x "+f), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Recursive listing.
	var keys []string
	for obj := range client.ListObjects(ctx, "data", minio.ListObjectsOptions{Recursive: true}) {
		if obj.Err != nil {
			t.Fatalf("ListObjects: %v", obj.Err)
		}
		keys = append(keys, obj.Key)
	}
	want := []string{"a.txt", "docs/readme.md", "docs/sub/deep.txt", "logs/app.log", "z.bin"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("recursive keys = %v, want %v", keys, want)
	}

	// Delimiter listing at root: files + common prefixes.
	keys = keys[:0]
	var prefixes []string
	for obj := range client.ListObjects(ctx, "data", minio.ListObjectsOptions{Recursive: false}) {
		if obj.Err != nil {
			t.Fatalf("ListObjects: %v", obj.Err)
		}
		if strings.HasSuffix(obj.Key, "/") {
			prefixes = append(prefixes, obj.Key)
		} else {
			keys = append(keys, obj.Key)
		}
	}
	if strings.Join(keys, ",") != "a.txt,z.bin" {
		t.Fatalf("root keys = %v, want [a.txt z.bin]", keys)
	}
	if strings.Join(prefixes, ",") != "docs/,logs/" {
		t.Fatalf("root prefixes = %v, want [docs/ logs/]", prefixes)
	}

	// Prefix listing.
	keys = keys[:0]
	for obj := range client.ListObjects(ctx, "data", minio.ListObjectsOptions{Prefix: "docs/", Recursive: true}) {
		if obj.Err != nil {
			t.Fatalf("ListObjects: %v", obj.Err)
		}
		keys = append(keys, obj.Key)
	}
	if strings.Join(keys, ",") != "docs/readme.md,docs/sub/deep.txt" {
		t.Fatalf("prefix keys = %v", keys)
	}
}

func TestListPagination(t *testing.T) {
	client, root := startTestServer(t)
	ctx := context.Background()

	for i := 0; i < 25; i++ {
		name := fmt.Sprintf("file-%03d.txt", i)
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var keys []string
	for obj := range client.ListObjects(ctx, "data", minio.ListObjectsOptions{Recursive: true, MaxKeys: 7}) {
		if obj.Err != nil {
			t.Fatalf("ListObjects: %v", obj.Err)
		}
		keys = append(keys, obj.Key)
	}
	if len(keys) != 25 {
		t.Fatalf("paginated listing returned %d keys, want 25", len(keys))
	}
	for i, k := range keys {
		if want := fmt.Sprintf("file-%03d.txt", i); k != want {
			t.Fatalf("key[%d] = %q, want %q", i, k, want)
		}
	}
}

func TestMultipartUpload(t *testing.T) {
	client, root := startTestServer(t)
	ctx := context.Background()

	// 11 MiB with 5 MiB parts forces a 3-part multipart upload.
	payload := make([]byte, 11<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	_, err := client.PutObject(ctx, "data", "big/blob.bin",
		bytes.NewReader(payload), int64(len(payload)),
		minio.PutObjectOptions{PartSize: 5 << 20})
	if err != nil {
		t.Fatalf("multipart PutObject: %v", err)
	}

	onDisk, err := os.ReadFile(filepath.Join(root, "big", "blob.bin"))
	if err != nil {
		t.Fatalf("read assembled file: %v", err)
	}
	if !bytes.Equal(onDisk, payload) {
		t.Fatalf("assembled content mismatch: %d bytes on disk, want %d", len(onDisk), len(payload))
	}
}

func TestCopyObject(t *testing.T) {
	client, root := startTestServer(t)
	ctx := context.Background()

	if err := os.WriteFile(filepath.Join(root, "src.txt"), []byte("copy me"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := client.CopyObject(ctx,
		minio.CopyDestOptions{Bucket: "data", Object: "copies/dst.txt"},
		minio.CopySrcOptions{Bucket: "data", Object: "src.txt"})
	if err != nil {
		t.Fatalf("CopyObject: %v", err)
	}

	onDisk, err := os.ReadFile(filepath.Join(root, "copies", "dst.txt"))
	if err != nil || string(onDisk) != "copy me" {
		t.Fatalf("copied content = %q, %v", onDisk, err)
	}
}

func TestRemoveObjectsBatch(t *testing.T) {
	client, root := startTestServer(t)
	ctx := context.Background()

	for _, name := range []string{"b1.txt", "b2.txt", "b3.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	objectsCh := make(chan minio.ObjectInfo, 3)
	objectsCh <- minio.ObjectInfo{Key: "b1.txt"}
	objectsCh <- minio.ObjectInfo{Key: "b2.txt"}
	close(objectsCh)

	for rErr := range client.RemoveObjects(ctx, "data", objectsCh, minio.RemoveObjectsOptions{}) {
		t.Fatalf("RemoveObjects error: %v", rErr.Err)
	}

	if _, err := os.Stat(filepath.Join(root, "b1.txt")); !os.IsNotExist(err) {
		t.Fatal("b1.txt should be deleted")
	}
	if _, err := os.Stat(filepath.Join(root, "b3.txt")); err != nil {
		t.Fatal("b3.txt should still exist")
	}
}

func TestPresignedGet(t *testing.T) {
	client, root := startTestServer(t)
	ctx := context.Background()

	if err := os.WriteFile(filepath.Join(root, "presigned.txt"), []byte("presigned body"), 0o644); err != nil {
		t.Fatal(err)
	}

	u, err := client.PresignedGetObject(ctx, "data", "presigned.txt", time.Minute, nil)
	if err != nil {
		t.Fatalf("PresignedGetObject: %v", err)
	}

	resp, err := http.Get(u.String())
	if err != nil {
		t.Fatalf("GET presigned: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("presigned GET status = %d, body: %s", resp.StatusCode, body)
	}
	if string(body) != "presigned body" {
		t.Fatalf("presigned body = %q", body)
	}
}

func TestAuthFailures(t *testing.T) {
	client, root := startTestServer(t)
	ctx := context.Background()

	if err := os.WriteFile(filepath.Join(root, "auth.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	endpoint := client.EndpointURL().Host

	// Wrong secret.
	bad, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4("testkey", "wrongsecret999", ""),
		Secure: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.StatObject(ctx, "data", "auth.txt", minio.StatObjectOptions{}); err == nil {
		t.Fatal("wrong secret should be rejected")
	}

	// Unknown access key.
	unknown, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4("nobody", "whatever12345", ""),
		Secure: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unknown.ListBuckets(ctx); err == nil {
		t.Fatal("unknown access key should be rejected")
	}

	// Anonymous.
	resp, err := http.Get("http://" + endpoint + "/data/auth.txt")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("anonymous GET status = %d, want 403", resp.StatusCode)
	}
}

func TestReadOnlyUser(t *testing.T) {
	client, root := startTestServer(t)
	ctx := context.Background()

	if err := os.WriteFile(filepath.Join(root, "ro.txt"), []byte("read only"), 0o644); err != nil {
		t.Fatal(err)
	}

	ro, err := minio.New(client.EndpointURL().Host, &minio.Options{
		Creds:  credentials.NewStaticV4("rokey", "rosecret1234", ""),
		Secure: false,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Reads work.
	obj, err := ro.GetObject(ctx, "data", "ro.txt", minio.GetObjectOptions{})
	if err != nil {
		t.Fatalf("read-only GetObject: %v", err)
	}
	if body, err := io.ReadAll(obj); err != nil || string(body) != "read only" {
		t.Fatalf("read-only read = %q, %v", body, err)
	}

	// Writes are denied.
	_, err = ro.PutObject(ctx, "data", "new.txt", strings.NewReader("nope"), 4, minio.PutObjectOptions{})
	if err == nil {
		t.Fatal("read-only user PutObject should fail")
	}
	if err := ro.RemoveObject(ctx, "data", "ro.txt", minio.RemoveObjectOptions{}); err == nil {
		t.Fatal("read-only user RemoveObject should fail")
	}
}

func TestKeysWithSpecialCharacters(t *testing.T) {
	client, _ := startTestServer(t)
	ctx := context.Background()

	keys := []string{
		"with space/file name.txt",
		"unicode/döşya-ğüş.txt",
		"plus+and=eq/x&y.txt",
	}

	for _, key := range keys {
		content := []byte("content of " + key)
		if _, err := client.PutObject(ctx, "data", key, bytes.NewReader(content), int64(len(content)), minio.PutObjectOptions{}); err != nil {
			t.Fatalf("PutObject(%q): %v", key, err)
		}
		obj, err := client.GetObject(ctx, "data", key, minio.GetObjectOptions{})
		if err != nil {
			t.Fatalf("GetObject(%q): %v", key, err)
		}
		body, err := io.ReadAll(obj)
		if err != nil || !bytes.Equal(body, content) {
			t.Fatalf("roundtrip(%q) = %q, %v", key, body, err)
		}
	}

	// Listing must return the exact keys.
	var listed []string
	for obj := range client.ListObjects(ctx, "data", minio.ListObjectsOptions{Recursive: true}) {
		if obj.Err != nil {
			t.Fatalf("ListObjects: %v", obj.Err)
		}
		listed = append(listed, obj.Key)
	}
	if len(listed) != len(keys) {
		t.Fatalf("listed %d keys, want %d: %v", len(listed), len(keys), listed)
	}
}

func TestTraversalRejected(t *testing.T) {
	client, root := startTestServer(t)
	ctx := context.Background()

	// A secret outside the share root must not be reachable.
	outside := filepath.Join(filepath.Dir(root), "outside-secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(outside) }) //nolint:errcheck

	_, err := client.GetObject(ctx, "data", "../outside-secret.txt", minio.GetObjectOptions{})
	if err == nil {
		obj, _ := client.GetObject(ctx, "data", "../outside-secret.txt", minio.GetObjectOptions{})
		if body, rerr := io.ReadAll(obj); rerr == nil && string(body) == "secret" {
			t.Fatal("path traversal leaked a file outside the share")
		}
	}
}
