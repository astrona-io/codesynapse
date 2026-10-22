package hugegraphstore

import (
	"context"
	"fmt"

	"github.com/parisnakitakejser/codesynapse/internal/model"
)

// Node property types. TestSchemaCoversNodeProps keeps them in step with
// package nodeprops; "id" is stored as "nid".
var (
	nodeTexts = []string{"nid", "kind", "repo", "name", "qualified_name", "package", "file", "signature", "summary", "body_hash", "clone_hash", "clone_sig", "via", "doc_type", "status", "meta", "duplicates",
		"time", "head", "root", "branch", "metrics", "severity", "rule", "version", "fixed_version", "reachability", "url",
		"liveness", "cleanup", "licenses", "coverage", "language", "ecosystem", "license", "license_source", "run"}
	nodeLongs = []string{"bytes", "start_line", "end_line", "complexity", "loc", "level", "churn", "roots"}
	nodeBools = []string{"exported", "is_test", "indirect", "dev"}
	nodeLists = []string{"cwe", "aliases", "security_tags"}
)

// Other labels' properties (names shared with nodes have the same type).
var (
	entityProps   = []string{"eid", "tenant_id", "ns", "name", "type", "aliases", "created_at", "updated_at", "emb_model"}
	factProps     = []string{"fid", "tenant_id", "ns", "subj", "obj", "predicate", "source", "confirmations", "confidence", "valid_from", "valid_to", "created_at", "updated_at"}
	reviewProps   = []string{"rid", "repo", "created_at", "updated_at", "doc"}
	snapshotProps = []string{"sid", "repo", "at", "doc"}
	usageProps    = []string{"ukey", "day", "daynum", "org", "repo", "tool", "client", "calls", "errors", "response_tokens", "duration_ms", "max_duration_ms"}
	otherTexts    = []string{"eid", "tenant_id", "ns", "type", "created_at", "updated_at", "emb_model", "fid", "subj", "obj", "predicate", "source",
		"valid_from", "valid_to", "rid", "doc", "ukey", "day", "org", "tool", "client", "sid", "at"}
	otherLongs = []string{"count", "confirmations", "daynum", "calls", "errors", "response_tokens", "duration_ms", "max_duration_ms"}
)

const schemaScript = `
def s = graph.schema()
texts.each { s.propertyKey(it).asText().ifNotExist().create() }
longs.each { s.propertyKey(it).asLong().ifNotExist().create() }
bools.each { s.propertyKey(it).asBoolean().ifNotExist().create() }
lists.each { s.propertyKey(it).asText().valueList().ifNotExist().create() }
s.propertyKey('confidence').asDouble().ifNotExist().create()

def label = { name, idKeys, props ->
  def nullable = props - idKeys
  s.vertexLabel(name).useCustomizeStringId().properties(props as String[]).nullableKeys(nullable as String[]).ifNotExist().create()
  // Labels created by an older version get new properties appended.
  def have = s.getVertexLabel(name).properties().collect { graph.propertyKey(it).name() }
  def missing = props - have
  if (missing) { s.vertexLabel(name).properties(missing as String[]).nullableKeys(missing as String[]).append() }
}
label('node', ['nid', 'repo', 'kind'], nodeProps)
label('mem_entity', ['eid', 'tenant_id', 'ns'], entityProps)
label('review', ['rid', 'repo'], reviewProps)
label('usage', ['ukey', 'daynum'], usageProps)
label('snapshot', ['sid', 'repo'], snapshotProps)

edgeKinds.each { k ->
  s.edgeLabel(k).sourceLabel('node').targetLabel('node').properties('count', 'run').nullableKeys('count', 'run').ifNotExist().create()
}
def factNullable = factProps - ['fid', 'tenant_id', 'ns', 'subj', 'obj']
s.edgeLabel('fact').sourceLabel('mem_entity').targetLabel('mem_entity').multiTimes().properties(factProps as String[])
  .sortKeys('fid').nullableKeys(factNullable as String[]).ifNotExist().create()

s.indexLabel('nodeByRepo').onV('node').by('repo').secondary().ifNotExist().create()
s.indexLabel('nodeByKind').onV('node').by('kind').secondary().ifNotExist().create()
s.indexLabel('nodeByFile').onV('node').by('file').secondary().ifNotExist().create()
s.indexLabel('memEntityByNs').onV('mem_entity').by('tenant_id', 'ns').secondary().ifNotExist().create()
s.indexLabel('factByFid').onE('fact').by('fid').secondary().ifNotExist().create()
s.indexLabel('factByNs').onE('fact').by('tenant_id', 'ns').secondary().ifNotExist().create()
s.indexLabel('reviewByRepo').onV('review').by('repo').secondary().ifNotExist().create()
s.indexLabel('snapshotByRepo').onV('snapshot').by('repo').secondary().ifNotExist().create()
s.indexLabel('usageByDay').onV('usage').by('daynum').range().ifNotExist().create()
1`

// EnsureSchema implements store.Writer. Property keys, labels and indexes
// are created when missing; nothing is dropped.
func (s *Store) EnsureSchema(ctx context.Context) error {
	edges := make([]string, len(model.AllEdgeKinds))
	for i, k := range model.AllEdgeKinds {
		edges[i] = string(k)
	}
	nodeProps := append(append(append(append([]string{}, nodeTexts...), nodeLongs...), nodeBools...), nodeLists...)
	err := s.write(ctx, schemaScript, map[string]any{
		"texts": append(append([]string{}, nodeTexts...), otherTexts...), "longs": append(append([]string{}, nodeLongs...), otherLongs...),
		"bools": nodeBools, "lists": nodeLists,
		"nodeProps": nodeProps, "entityProps": entityProps, "factProps": factProps, "reviewProps": reviewProps, "snapshotProps": snapshotProps, "usageProps": usageProps,
		"edgeKinds": edges,
	}, nil)
	if err != nil {
		return fmt.Errorf("hugegraph schema: %w", err)
	}
	return s.write(ctx, accessSchema, nil, nil)
}
