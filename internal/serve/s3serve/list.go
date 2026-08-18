package s3serve

import (
	"sort"
	"strings"

	"github.com/rakunlabs/kutu/internal/serve/ftpserve"
)

// listEntry is one result row of a bucket listing: either an object or
// a common prefix (isPrefix). Keys of common prefixes end with "/".
type listEntry struct {
	key      string
	size     int64
	isPrefix bool
	src      *ftpserve.ShareSource
}

// listShare lists a share's contents in S3 key order.
//
// prefix filters keys, delimiter is "" (recursive) or "/" (single
// level), after is the exclusive lexicographic starting point
// (continuation token / marker / start-after) and max caps the combined
// number of objects + common prefixes. The second return value reports
// whether more results exist beyond the returned page.
func listShare(share *ftpserve.Share, prefix, delimiter, after string, max int) ([]listEntry, bool) {
	if max <= 0 {
		max = 1000
	}

	var all []listEntry
	for i := range share.Sources {
		src := &share.Sources[i]
		all = append(all, collectSource(src, prefix, delimiter, after, max+1)...)
	}

	sort.Slice(all, func(i, j int) bool { return all[i].key < all[j].key })

	// Dedup by key across sources; the first source wins.
	deduped := all[:0]
	var last string
	for _, e := range all {
		if e.key == last && len(deduped) > 0 {
			continue
		}
		last = e.key
		deduped = append(deduped, e)
	}

	truncated := len(deduped) > max
	if truncated {
		deduped = deduped[:max]
	}
	return deduped, truncated
}

// collectSource gathers up to limit entries from a single share source.
func collectSource(src *ftpserve.ShareSource, prefix, delimiter, after string, limit int) []listEntry {
	if delimiter == "/" {
		return collectSingleLevel(src, prefix, after, limit)
	}
	return collectRecursive(src, prefix, after, limit)
}

// collectSingleLevel lists one directory level, emitting sub-directories
// as common prefixes (S3 delimiter="/" semantics).
func collectSingleLevel(src *ftpserve.ShareSource, prefix, after string, limit int) []listEntry {
	scanDir, namePfx := splitPrefixDir(prefix)

	entries, err := src.FS.ReadDir(sourceFSPath(src, scanDir))
	if err != nil {
		return nil
	}

	out := make([]listEntry, 0, len(entries))
	for _, e := range entries {
		if !strings.HasPrefix(e.Name, namePfx) {
			continue
		}
		key := joinKey(scanDir, e.Name)
		if e.IsDir {
			key += "/"
			// Pagination tokens are always previously emitted elements
			// (object keys or common prefixes), so anything sorting at
			// or before the marker has already been returned.
			if key <= after {
				continue
			}
			out = append(out, listEntry{key: key, isPrefix: true, src: src})
		} else {
			if key <= after {
				continue
			}
			out = append(out, listEntry{key: key, size: e.Size, src: src})
		}
		if len(out) >= limit {
			break
		}
	}

	sortEntries(out)
	return out
}

// collectRecursive walks the tree under prefix in key order.
func collectRecursive(src *ftpserve.ShareSource, prefix, after string, limit int) []listEntry {
	var out []listEntry

	startDir, _ := splitPrefixDir(prefix)

	var walk func(dir string)
	walk = func(dir string) {
		if len(out) >= limit {
			return
		}
		entries, err := src.FS.ReadDir(sourceFSPath(src, dir))
		if err != nil {
			return
		}

		type item struct {
			name  string
			isDir bool
			size  int64
			key   string // sort key: dirs get a trailing "/"
		}
		items := make([]item, 0, len(entries))
		for _, e := range entries {
			it := item{name: e.Name, isDir: e.IsDir, size: e.Size}
			it.key = joinKey(dir, e.Name)
			if e.IsDir {
				it.key += "/"
			}
			items = append(items, it)
		}
		sort.Slice(items, func(i, j int) bool { return items[i].key < items[j].key })

		for _, it := range items {
			if len(out) >= limit {
				return
			}
			if it.isDir {
				if dirMayContain(it.key, prefix, after) {
					walk(joinKey(dir, it.name))
				}
				continue
			}
			key := joinKey(dir, it.name)
			if !strings.HasPrefix(key, prefix) || key <= after {
				continue
			}
			out = append(out, listEntry{key: key, size: it.size, src: src})
		}
	}

	walk(startDir)
	return out
}

// dirMayContain reports whether the directory (dirKey ends with "/")
// can hold keys matching prefix that sort after the given marker.
func dirMayContain(dirKey, prefix, after string) bool {
	if !strings.HasPrefix(dirKey, prefix) && !strings.HasPrefix(prefix, dirKey) {
		return false
	}
	if after == "" || after < dirKey || strings.HasPrefix(after, dirKey) {
		return true
	}
	return false
}

// splitPrefixDir splits an S3 key prefix into the directory to scan and
// the entry-name prefix within it. "foo/ba" -> ("foo", "ba").
func splitPrefixDir(prefix string) (dir, namePfx string) {
	if i := strings.LastIndexByte(prefix, '/'); i >= 0 {
		return prefix[:i], prefix[i+1:]
	}
	return "", prefix
}

func joinKey(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func sortEntries(entries []listEntry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].key < entries[j].key })
}
