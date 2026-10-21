package golang

import (
	"slices"
	"strings"
)

// Security tags attached to functions that call sensitive APIs. They drive
// review focus and the dashboard's security view; they are signals for a
// reviewer, not vulnerability findings.
const (
	TagExec          = "exec"
	TagSQL           = "sql"
	TagCypher        = "cypher"
	TagHTTP          = "http"
	TagNetwork       = "network"
	TagFilesystem    = "filesystem"
	TagCrypto        = "crypto"
	TagWeakCrypto    = "weak-crypto"
	TagInsecureRand  = "insecure-rand"
	TagTLS           = "tls"
	TagUnsafe        = "unsafe"
	TagReflect       = "reflect"
	TagDeserialize   = "deserialize"
	TagTemplate      = "template"
	TagEnv           = "env"
	TagSyscall       = "syscall"
	TagAuthorization = "auth"
)

// pkgTags maps an import path (or prefix ending in "/") to a tag that applies
// to every call into it.
var pkgTags = map[string]string{
	"os/exec":                           TagExec,
	"syscall":                           TagSyscall,
	"golang.org/x/sys/unix":             TagSyscall,
	"unsafe":                            TagUnsafe,
	"reflect":                           TagReflect,
	"crypto/md5":                        TagWeakCrypto,
	"crypto/sha1":                       TagWeakCrypto,
	"crypto/des":                        TagWeakCrypto,
	"crypto/rc4":                        TagWeakCrypto,
	"crypto/tls":                        TagTLS,
	"crypto/":                           TagCrypto,
	"golang.org/x/crypto/":              TagCrypto,
	"math/rand":                         TagInsecureRand,
	"math/rand/v2":                      TagInsecureRand,
	"net/http":                          TagHTTP,
	"net":                               TagNetwork,
	"encoding/gob":                      TagDeserialize,
	"encoding/xml":                      TagDeserialize,
	"gopkg.in/yaml.v3":                  TagDeserialize,
	"sigs.k8s.io/yaml":                  TagDeserialize,
	"text/template":                     TagTemplate,
	"html/template":                     TagTemplate,
	"github.com/golang-jwt/jwt/":        TagAuthorization,
	"github.com/neo4j/neo4j-go-driver/": TagCypher,
	"gorm.io/gorm":                      TagSQL,
	"github.com/jackc/pgx/":             TagSQL,
}

// funcTags maps a fully qualified function name (types.Func.FullName) to a
// tag, for packages where only some functions are sensitive.
var funcTags = map[string]string{
	"os.Getenv":                          TagEnv,
	"os.LookupEnv":                       TagEnv,
	"os.Environ":                         TagEnv,
	"os.Open":                            TagFilesystem,
	"os.OpenFile":                        TagFilesystem,
	"os.Create":                          TagFilesystem,
	"os.ReadFile":                        TagFilesystem,
	"os.WriteFile":                       TagFilesystem,
	"os.Remove":                          TagFilesystem,
	"os.RemoveAll":                       TagFilesystem,
	"os.Chmod":                           TagFilesystem,
	"os.MkdirAll":                        TagFilesystem,
	"os.ReadDir":                         TagFilesystem,
	"path/filepath.Join":                 TagFilesystem,
	"(*database/sql.DB).Query":           TagSQL,
	"(*database/sql.DB).QueryContext":    TagSQL,
	"(*database/sql.DB).QueryRow":        TagSQL,
	"(*database/sql.DB).QueryRowContext": TagSQL,
	"(*database/sql.DB).Exec":            TagSQL,
	"(*database/sql.DB).ExecContext":     TagSQL,
	"(*database/sql.Tx).Query":           TagSQL,
	"(*database/sql.Tx).QueryContext":    TagSQL,
	"(*database/sql.Tx).Exec":            TagSQL,
	"(*database/sql.Tx).ExecContext":     TagSQL,
	"(*database/sql.DB).Prepare":         TagSQL,
	"(*database/sql.DB).PrepareContext":  TagSQL,
	"encoding/json.Unmarshal":            TagDeserialize,
	"(*encoding/json.Decoder).Decode":    TagDeserialize,
}

// tagFor returns the security tag for a call to the function with the given
// package path and full name, or "".
func tagFor(pkgPath, fullName string) string {
	if t, ok := funcTags[fullName]; ok {
		return t
	}
	if t, ok := pkgTags[pkgPath]; ok {
		return t
	}
	// Longest matching prefix wins so "crypto/tls" beats "crypto/".
	best, bestLen := "", 0
	for prefix, t := range pkgTags {
		if strings.HasSuffix(prefix, "/") && strings.HasPrefix(pkgPath, prefix) && len(prefix) > bestLen {
			best, bestLen = t, len(prefix)
		}
	}
	return best
}

func addTag(tags []string, tag string) []string {
	if tag == "" || slices.Contains(tags, tag) {
		return tags
	}
	tags = append(tags, tag)
	slices.Sort(tags)
	return tags
}
