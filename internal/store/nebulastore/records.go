package nebulastore

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/astrona-io/codesynapse/internal/curation"
	"github.com/astrona-io/codesynapse/internal/history"
	"github.com/astrona-io/codesynapse/internal/reviews"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/nodeprops"
	"github.com/astrona-io/codesynapse/internal/usage"
)

var (
	_ store.ReviewStore  = (*Store)(nil)
	_ store.HistoryStore = (*Store)(nil)
	_ store.CuratedStore = (*Store)(nil)
	_ store.UsageStore   = (*Store)(nil)
)

// Reviews, snapshots and curated items are tags of their own, so repository
// replacement and deletion (tag "node") never touch them. Each is one JSON
// document with the few properties it is found by beside it.

// getDoc reads the document of one vertex of tag.
func (s *Store) getDoc(ctx context.Context, tag, vertex string) (string, bool, error) {
	rows, err := s.read(ctx, "FETCH PROP ON "+quote(tag)+" "+literal(vertex)+" YIELD properties(vertex).doc AS doc", nil)
	if err != nil || len(rows) == 0 {
		return "", false, err
	}
	doc, _ := rows[0]["doc"].(string)
	return doc, true, nil
}

// dropWhere deletes every vertex of tag matching a constant condition on its
// repo property, in batches.
func (s *Store) dropWhere(ctx context.Context, tag, repo string) error {
	stmt := "MATCH (v:" + quote(tag) + ") WHERE v." + quote(tag) + ".`repo` == $repo RETURN id(v) AS id LIMIT 400"
	for {
		rows, err := s.read(ctx, stmt, map[string]any{"repo": repo})
		if err != nil || len(rows) == 0 {
			return err
		}
		ids := make([]string, len(rows))
		for i, r := range rows {
			ids[i], _ = r["id"].(string)
		}
		if _, err := s.write(ctx, "DELETE VERTEX "+literals(ids), nil); err != nil {
			return err
		}
	}
}

func reviewVID(id string) string { return vid("r", id) }

// SaveReview implements store.ReviewStore.
func (s *Store) SaveReview(ctx context.Context, r reviews.Review) error {
	doc, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.write(ctx, "INSERT VERTEX `review` (`rid`,`repo`,`created_at`,`updated_at`,`doc`) VALUES "+literal(reviewVID(r.ID))+":($rid,$repo,$created,$updated,$doc)",
		map[string]any{"rid": r.ID, "repo": r.Repo, "created": r.CreatedAt, "updated": r.UpdatedAt, "doc": string(doc)})
	return err
}

// GetReview implements store.ReviewStore.
func (s *Store) GetReview(ctx context.Context, id string) (reviews.Review, error) {
	doc, ok, err := s.getDoc(ctx, "review", reviewVID(id))
	if err != nil {
		return reviews.Review{}, err
	}
	if !ok {
		return reviews.Review{}, fmt.Errorf("review %q: %w", id, store.ErrNotFound)
	}
	var r reviews.Review
	err = json.Unmarshal([]byte(doc), &r)
	return r, err
}

// ListReviews implements store.ReviewStore.
func (s *Store) ListReviews(ctx context.Context, repo string, limit int) ([]reviews.Review, error) {
	rows, err := s.read(ctx, "MATCH (v:`review`) WHERE v.`review`.`repo` == $repo RETURN v.`review`.`doc` AS doc, v.`review`.`created_at` AS at, v.`review`.`rid` AS rid"+
		" ORDER BY at DESC, rid LIMIT $limit", map[string]any{"repo": repo, "limit": int64(store.ClampLimit(limit, 50))})
	if err != nil {
		return nil, err
	}
	out := make([]reviews.Review, 0, len(rows))
	for _, row := range rows {
		var r reviews.Review
		doc, _ := row["doc"].(string)
		if err := json.Unmarshal([]byte(doc), &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// DeleteReview implements store.ReviewStore.
func (s *Store) DeleteReview(ctx context.Context, id string) error {
	_, err := s.write(ctx, "DELETE VERTEX "+literal(reviewVID(id)), nil)
	return err
}

func curatedVID(repo, id string) string { return vid("c", repo+"|"+id) }

// SaveCurated implements store.CuratedStore.
func (s *Store) SaveCurated(ctx context.Context, it curation.Item) error {
	doc, err := json.Marshal(it)
	if err != nil {
		return err
	}
	_, err = s.write(ctx, "INSERT VERTEX `curated` (`cid`,`repo`,`kind`,`created_at`,`doc`) VALUES "+literal(curatedVID(it.Repo, it.ID))+":($cid,$repo,$kind,$created,$doc)",
		map[string]any{"cid": it.ID, "repo": it.Repo, "kind": it.Kind, "created": it.CreatedAt, "doc": string(doc)})
	return err
}

// GetCurated implements store.CuratedStore.
func (s *Store) GetCurated(ctx context.Context, repo, id string) (curation.Item, error) {
	doc, ok, err := s.getDoc(ctx, "curated", curatedVID(repo, id))
	if err != nil {
		return curation.Item{}, err
	}
	if !ok {
		return curation.Item{}, fmt.Errorf("item %q: %w", id, store.ErrNotFound)
	}
	var it curation.Item
	err = json.Unmarshal([]byte(doc), &it)
	return it, err
}

// ListCurated implements store.CuratedStore.
func (s *Store) ListCurated(ctx context.Context, repo, kind string, limit int) ([]curation.Item, error) {
	rows, err := s.read(ctx, "MATCH (v:`curated`) WHERE v.`curated`.`repo` == $repo AND ($kind == \"\" OR v.`curated`.`kind` == $kind)"+
		" RETURN v.`curated`.`doc` AS doc, v.`curated`.`created_at` AS at, v.`curated`.`cid` AS cid ORDER BY at DESC, cid LIMIT $limit",
		map[string]any{"repo": repo, "kind": kind, "limit": int64(store.ClampLimit(limit, 100))})
	if err != nil {
		return nil, err
	}
	out := make([]curation.Item, 0, len(rows))
	for _, row := range rows {
		var it curation.Item
		doc, _ := row["doc"].(string)
		if err := json.Unmarshal([]byte(doc), &it); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, nil
}

// DeleteCurated implements store.CuratedStore.
func (s *Store) DeleteCurated(ctx context.Context, repo, id string) error {
	_, err := s.write(ctx, "DELETE VERTEX "+literal(curatedVID(repo, id)), nil)
	return err
}

// DeleteAllCurated implements store.CuratedStore.
func (s *Store) DeleteAllCurated(ctx context.Context, repo string) error {
	return s.dropWhere(ctx, "curated", repo)
}

func snapshotVID(repo, at string) string { return vid("s", repo+"|"+at) }

// AddSnapshot implements store.HistoryStore.
func (s *Store) AddSnapshot(ctx context.Context, repo string, snap history.Snapshot) error {
	doc, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	v := snapshotVID(repo, snap.At)
	if _, err := s.write(ctx, "INSERT VERTEX `snapshot` (`sid`,`repo`,`at`,`doc`) VALUES "+literal(v)+":($sid,$repo,$at,$doc)",
		map[string]any{"sid": v, "repo": repo, "at": snap.At, "doc": string(doc)}); err != nil {
		return err
	}
	// Keep the newest history.MaxSnapshots.
	rows, err := s.read(ctx, "MATCH (v:`snapshot`) WHERE v.`snapshot`.`repo` == $repo RETURN id(v) AS id, v.`snapshot`.`at` AS at ORDER BY at DESC SKIP $keep LIMIT 400",
		map[string]any{"repo": repo, "keep": int64(history.MaxSnapshots)})
	if err != nil || len(rows) == 0 {
		return err
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i], _ = r["id"].(string)
	}
	_, err = s.write(ctx, "DELETE VERTEX "+literals(ids), nil)
	return err
}

// Snapshots implements store.HistoryStore.
func (s *Store) Snapshots(ctx context.Context, repo string, limit int) ([]history.Snapshot, error) {
	rows, err := s.read(ctx, "MATCH (v:`snapshot`) WHERE v.`snapshot`.`repo` == $repo RETURN v.`snapshot`.`doc` AS doc, v.`snapshot`.`at` AS at ORDER BY at DESC LIMIT $limit",
		map[string]any{"repo": repo, "limit": int64(store.ClampLimit(limit, 100))})
	if err != nil {
		return nil, err
	}
	out := make([]history.Snapshot, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- { // newest first in the store, oldest first here
		var snap history.Snapshot
		doc, _ := rows[i]["doc"].(string)
		if err := json.Unmarshal([]byte(doc), &snap); err != nil {
			return nil, err
		}
		out = append(out, snap)
	}
	return out, nil
}

// DeleteSnapshots implements store.HistoryStore.
func (s *Store) DeleteSnapshots(ctx context.Context, repo string) error {
	return s.dropWhere(ctx, "snapshot", repo)
}

// dayNum turns YYYY-MM-DD into YYYYMMDD for the range index.
func dayNum(day string) (int64, error) {
	if len(day) != 10 || day[4] != '-' || day[7] != '-' {
		return 0, fmt.Errorf("invalid day %q", day)
	}
	return strconv.ParseInt(strings.ReplaceAll(day, "-", ""), 10, 64)
}

// AddUsage implements store.UsageStore. Each bucket is one UPSERT that adds
// to the stored counters on the server, so the MCP recorder and the API can
// add to the same bucket without reading it first.
func (s *Store) AddUsage(ctx context.Context, buckets []usage.Bucket) error {
	for _, b := range buckets {
		n, err := dayNum(b.Day)
		if err != nil {
			return err
		}
		params := map[string]any{
			"key": b.Key(), "day": b.Day, "daynum": n, "hour": b.Hour, "org": b.Org, "repo": b.Repo, "tool": b.Tool, "client": b.Client,
			"calls": int64(b.Calls), "errors": int64(b.Errors), "timeouts": int64(b.Timeouts), "tokens": int64(b.ResponseTokens),
			"ms": int64(b.DurationMs), "max": int64(b.MaxDurationMs), "min": int64(b.MinDurationMs), "timed": int64(b.Latency.Count()),
			"returned": int64(b.RangesReturned), "read": int64(b.RangesRead),
		}
		for i, c := range b.Latency {
			params["l"+strconv.Itoa(i)] = int64(c)
		}
		if _, err := s.write(ctx, fmt.Sprintf(usageUpsert, literal(vid("u", b.Key()))), params); err != nil {
			return err
		}
	}
	return nil
}

// usageUpsert adds a bucket to the stored counters on the server. The
// minimum comes first, so it compares with the histogram as stored before
// this bucket; it only counts once the histogram holds a call.
var usageUpsert = func() string {
	lat := usage.LatencyColumns()
	sum := make([]string, len(lat))
	add := make([]string, len(lat))
	for i, c := range lat {
		sum[i] = quote(c)
		add[i] = sum[i] + " = " + sum[i] + " + $l" + strconv.Itoa(i)
	}
	return "UPSERT VERTEX ON `usage` %s SET" +
		" `min_duration_ms` = CASE WHEN $timed > 0 AND (" + strings.Join(sum, " + ") + " == 0 OR $min < `min_duration_ms`) THEN $min ELSE `min_duration_ms` END," +
		" `ukey` = $key, `day` = $day, `daynum` = $daynum, `hour` = $hour, `org` = $org, `repo` = $repo, `tool` = $tool, `client` = $client," +
		" `calls` = `calls` + $calls, `errors` = `errors` + $errors, `timeouts` = `timeouts` + $timeouts, `response_tokens` = `response_tokens` + $tokens," +
		" `duration_ms` = `duration_ms` + $ms, `max_duration_ms` = CASE WHEN $max > `max_duration_ms` THEN $max ELSE `max_duration_ms` END," +
		" `ranges_returned` = `ranges_returned` + $returned, `ranges_read` = `ranges_read` + $read, " + strings.Join(add, ", ")
}()

// PruneUsage implements store.UsageStore: day and hourly buckets before the day.
func (s *Store) PruneUsage(ctx context.Context, beforeDay string) error {
	return s.pruneUsage(ctx, "MATCH (v:`usage`) WHERE v.`usage`.`daynum` < $day RETURN id(v) AS id LIMIT 400", beforeDay)
}

// PruneUsageHours implements store.UsageStore: hourly buckets only.
func (s *Store) PruneUsageHours(ctx context.Context, beforeDay string) error {
	return s.pruneUsage(ctx, "MATCH (v:`usage`) WHERE v.`usage`.`daynum` < $day AND v.`usage`.`hour` != \"\" RETURN id(v) AS id LIMIT 400", beforeDay)
}

func (s *Store) pruneUsage(ctx context.Context, stmt, beforeDay string) error {
	n, err := dayNum(beforeDay)
	if err != nil {
		return err
	}
	for {
		rows, err := s.read(ctx, stmt, map[string]any{"day": n})
		if err != nil || len(rows) == 0 {
			return err
		}
		ids := make([]string, len(rows))
		for i, r := range rows {
			ids[i], _ = r["id"].(string)
		}
		if _, err := s.write(ctx, "DELETE VERTEX "+literals(ids), nil); err != nil {
			return err
		}
	}
}

// Usage implements store.UsageStore.
func (s *Store) Usage(ctx context.Context, sinceDay string) ([]usage.Bucket, error) {
	return s.usageSince(ctx, "MATCH (v:`usage`) WHERE v.`usage`.`daynum` >= $day AND v.`usage`.`hour` == \"\" RETURN properties(v) AS p", sinceDay)
}

// UsageHours implements store.UsageStore.
func (s *Store) UsageHours(ctx context.Context, sinceDay string) ([]usage.Bucket, error) {
	return s.usageSince(ctx, "MATCH (v:`usage`) WHERE v.`usage`.`daynum` >= $day AND v.`usage`.`hour` != \"\" RETURN properties(v) AS p", sinceDay)
}

func (s *Store) usageSince(ctx context.Context, stmt, sinceDay string) ([]usage.Bucket, error) {
	n, err := dayNum(sinceDay)
	if err != nil {
		return nil, err
	}
	rows, err := s.read(ctx, stmt, map[string]any{"day": n})
	if err != nil {
		return nil, err
	}
	lat := usage.LatencyColumns()
	out := make([]usage.Bucket, 0, len(rows))
	for _, r := range rows {
		p, _ := r["p"].(map[string]any)
		str := func(k string) string { v, _ := p[k].(string); return v }
		b := usage.Bucket{Day: str("day"), Hour: str("hour"), Org: str("org"), Repo: str("repo"), Tool: str("tool"), Client: str("client"),
			Calls: nodeprops.Int(p["calls"]), Errors: nodeprops.Int(p["errors"]), Timeouts: nodeprops.Int(p["timeouts"]),
			ResponseTokens: nodeprops.Int(p["response_tokens"]), DurationMs: nodeprops.Int(p["duration_ms"]),
			MinDurationMs: nodeprops.Int(p["min_duration_ms"]), MaxDurationMs: nodeprops.Int(p["max_duration_ms"]),
			RangesReturned: nodeprops.Int(p["ranges_returned"]), RangesRead: nodeprops.Int(p["ranges_read"])}
		for i, c := range lat {
			b.Latency[i] = nodeprops.Int(p[c])
		}
		out = append(out, b)
	}
	slices.SortFunc(out, usage.Compare)
	return out, nil
}

var _ store.Store = (*Store)(nil)
