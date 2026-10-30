package fetchsource

import "net/url"

// Cached reports whether the cache folder dir holds an entry for u, a URL
// Normalise returned, whether fresh or stale, made by the current cleaner.
// It only reads: the folder is not created, secured or touched, so a lookup
// changes neither the cache nor its eviction order. The plugin's WebFetch
// hook uses it to point the agent at fetch_source for a page fetch_source
// already has. An entry an older cleaner made does not count: fetch_source
// downloads that page again, so saying it answers from the cache would be
// wrong.
func Cached(dir string, u *url.URL) bool {
	if dir == "" || u == nil {
		return false
	}
	e, ok := (&Cache{dir: dir}).Get(Key(u), u.String())
	return ok && e.Cleaner == CleanerVersion
}
