package store_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestSiteBackupSchedule(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		group := createGroup(ctx, t, db)
		sites := store.NewSites(db)
		s := newTestSite(group, "acme", 20000)
		if err := sites.Create(ctx, s); err != nil {
			t.Fatal(err)
		}

		got, err := sites.BackupSchedule(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		if want := store.DefaultSiteBackupSchedule(); !reflect.DeepEqual(got, want) {
			t.Errorf("new site's schedule = %+v, want %+v", got, want)
		}

		hour := 23
		for _, want := range []store.SiteBackupSchedule{
			{Enabled: true, IntervalHours: 12, Hour: &hour},
			{Enabled: false, IntervalHours: 168},
			store.DefaultSiteBackupSchedule(),
		} {
			ok, err := sites.SetBackupSchedule(ctx, s.ID, want)
			if err != nil || !ok {
				t.Fatalf("SetBackupSchedule(%+v) = %t, %v", want, ok, err)
			}
			got, err := sites.BackupSchedule(ctx, s.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("BackupSchedule = %+v, want %+v", got, want)
			}
		}

		missing := id.New()
		if _, err := sites.BackupSchedule(ctx, missing); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("BackupSchedule of a missing site: err = %v, want ErrNotFound", err)
		}
		if ok, err := sites.SetBackupSchedule(ctx, missing, store.DefaultSiteBackupSchedule()); ok || err != nil {
			t.Errorf("SetBackupSchedule of a missing site = %t, %v; want false, nil", ok, err)
		}
	})
}
