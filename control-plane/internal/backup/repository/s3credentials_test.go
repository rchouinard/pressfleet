package repository

import "testing"

func TestS3CredentialsFile(t *testing.T) {
	got, err := s3CredentialsFile(S3Config{AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "se/cr+et"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "[default]\naws_access_key_id = AKIAEXAMPLE\naws_secret_access_key = se/cr+et\n"; got.Reveal() != want {
		t.Errorf("credentials file = %q, want %q", got.Reveal(), want)
	}
	// A line break would add lines (another profile, a different key) to the
	// file.
	for _, bad := range []S3Config{
		{AccessKeyID: "AKIA\n[evil]", SecretAccessKey: "x"},
		{AccessKeyID: "AKIA", SecretAccessKey: "x\raws_session_token = y"},
		{AccessKeyID: "AKIA", SecretAccessKey: "x\x00"},
	} {
		if _, err := s3CredentialsFile(bad); err == nil {
			t.Errorf("credentials %+v accepted", bad)
		}
	}
}
