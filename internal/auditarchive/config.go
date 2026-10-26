package auditarchive

import (
	"errors"
	"strconv"
)

// Settings, from the environment only (the S3 keys are secrets).
const (
	EnvDir          = "CODESYNAPSE_AUDIT_ARCHIVE_DIR"
	EnvS3Endpoint   = "CODESYNAPSE_AUDIT_S3_ENDPOINT"
	EnvS3Bucket     = "CODESYNAPSE_AUDIT_S3_BUCKET"
	EnvS3Region     = "CODESYNAPSE_AUDIT_S3_REGION"
	EnvS3Prefix     = "CODESYNAPSE_AUDIT_S3_PREFIX"
	EnvS3LockMode   = "CODESYNAPSE_AUDIT_S3_LOCK_MODE"
	EnvS3AllowHTTP  = "CODESYNAPSE_AUDIT_S3_ALLOW_HTTP"
	EnvS3AccessKey  = "CODESYNAPSE_AUDIT_S3_ACCESS_KEY_ID"
	EnvS3SecretKey  = "CODESYNAPSE_AUDIT_S3_SECRET_ACCESS_KEY" //nolint:gosec // env var name, not a credential
	EnvS3SessionTok = "CODESYNAPSE_AUDIT_S3_SESSION_TOKEN"     //nolint:gosec // env var name, not a credential
)

// FromEnv returns the configured archive, or nil when none is: then old
// entries are deleted at the end of retention, as before.
func FromEnv(getenv func(string) string) (Archive, error) {
	dir, endpoint := getenv(EnvDir), getenv(EnvS3Endpoint)
	switch {
	case dir != "" && endpoint != "":
		return nil, errors.New(EnvDir + " and " + EnvS3Endpoint + " are mutually exclusive")
	case dir != "":
		return NewDir(dir)
	case endpoint == "":
		return nil, nil //nolint:nilnil // no archive configured
	}
	or := func(k, def string) string {
		if v := getenv(k); v != "" {
			return v
		}
		return def
	}
	allowHTTP, _ := strconv.ParseBool(getenv(EnvS3AllowHTTP)) //nolint:errcheck // anything else means false
	s := &S3{Endpoint: endpoint, Bucket: getenv(EnvS3Bucket), Region: or(EnvS3Region, "us-east-1"), Prefix: or(EnvS3Prefix, "audit/"),
		AccessKey: getenv(EnvS3AccessKey), SecretKey: getenv(EnvS3SecretKey), SessionToken: getenv(EnvS3SessionTok), LockMode: getenv(EnvS3LockMode)}
	if err := s.Check(allowHTTP); err != nil {
		return nil, err
	}
	return s, nil
}
