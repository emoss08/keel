package keel_test

import (
	"context"
	"strings"
	"testing"

	"github.com/emoss08/keel"
)

func settingsDB(t *testing.T, rec *recorder) *keel.DB {
	t.Helper()
	db(t)
	cfg := testConfig(testDSN)
	cfg.Observers = []keel.Observer{rec}
	cfg.TenantSettings = []string{"keel.org", "keel.bu"}
	d, err := keel.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return d
}

func settingsOf(t *testing.T, ctx context.Context, d *keel.DB) string {
	t.Helper()
	return must(keel.QueryValue[string](ctx, d,
		"SELECT coalesce(current_setting('keel.org', true), '') || '/' || coalesce(current_setting('keel.bu', true), '')"))(t)
}

func TestTenantSettingsFollowTheScopeInOneRoundTrip(t *testing.T) {
	rec := &recorder{}
	d := settingsDB(t, rec)
	ctx := newTenant()
	tn := ctx.Value(tenantKey{}).(tenant)
	want := string(tn.org) + "/" + string(tn.bu)

	rec.reset()
	eq(t, settingsOf(t, ctx, d), want)
	eq(t, rec.trips(), 1)

	var inWrite, inRead string
	ok(t, keel.Write(ctx, d, func(ctx context.Context) error {
		inWrite = settingsOf(t, ctx, d)
		return nil
	}))
	ok(t, keel.Read(ctx, d, func(ctx context.Context) error {
		inRead = settingsOf(t, ctx, d)
		return nil
	}))
	eq(t, inWrite, want)
	eq(t, inRead, want)

	other := newTenant()
	ot := other.Value(tenantKey{}).(tenant)
	eq(t, settingsOf(t, other, d), string(ot.org)+"/"+string(ot.bu))
}

func TestTenantSettingsAreNotSetForASystemScope(t *testing.T) {
	d := settingsDB(t, &recorder{})

	eq(t, settingsOf(t, systemCtx("nightly sweep"), d), "/")
	ok(t, keel.Read(systemCtx("nightly sweep"), d, func(ctx context.Context) error {
		eq(t, settingsOf(t, ctx, d), "/")
		return nil
	}))
}

func TestOpenRejectsTenantSettingsThatDoNotMatchTheColumns(t *testing.T) {
	cfg := testConfig("postgres://localhost/keel")
	cfg.TenantSettings = []string{"keel.org"}

	_, err := keel.Open(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "Config.TenantSettings names 1 settings for 2 tenant columns") {
		t.Fatalf("got %v", err)
	}
}
