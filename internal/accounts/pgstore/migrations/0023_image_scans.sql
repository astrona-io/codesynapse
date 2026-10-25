-- The last container image scan (Trivy or Grype report from CI) of a
-- repository namespace, "org|graph": parsed findings only (internal/imagescan),
-- replaced whole by the next upload.
CREATE TABLE image_scans (
    ns          text PRIMARY KEY,
    uploaded_at timestamptz NOT NULL,
    findings    jsonb NOT NULL
);
