package packages

import (
	"context"
	"strings"
	"testing"

	"github.com/lvlie/linux-ha-agent/internal/osinfo"
	"github.com/lvlie/linux-ha-agent/internal/runner"
)

// Output of a DS1520+ with DSM 7.4.1.
const (
	synopkgPlex = `[{"beta":false,"id":"PlexMediaServer","name":"Plex Media Server","version":"1.43.5.11079-720011079"}]` + "\n"
	dsmCurrent  = `{
   "data" : {
      "available" : false
   },
   "httpd_restart" : false,
   "success" : true
}`
	dsmNewer = `{"data":{"available":true,"version":"DSM 7.4.2-90100"},"success":true}`
)

func TestDSMCheck(t *testing.T) {
	f := (&runner.Fake{}).
		On("/usr/syno/bin/synopkg checkupdateall", runner.Result{Stdout: synopkgPlex}).
		On("SYNO.Core.Upgrade.Server", runner.Result{Stdout: dsmCurrent})
	m, err := For(osinfo.Info{Family: osinfo.FamilyDSM}, f, Options{})
	if err != nil {
		t.Fatal(err)
	}
	st, err := m.Check(context.Background())
	if err != nil || st.Manager != "dsm" || len(st.Updates) != 1 || st.Security != -1 {
		t.Fatalf("got %+v %v", st, err)
	}
	if u := st.Updates[0]; u.Name != "Plex Media Server" || u.Version != "1.43.5.11079-720011079" {
		t.Fatalf("update %+v", u)
	}
}

func TestDSMSystemUpdate(t *testing.T) {
	u, err := ParseDSMUpdateCheck(dsmNewer)
	if err != nil || u == nil || u.Name != "DSM" || u.Version != "DSM 7.4.2-90100" {
		t.Fatalf("got %+v %v", u, err)
	}
	if u, err := ParseDSMUpdateCheck(dsmCurrent); err != nil || u != nil {
		t.Fatalf("current DSM: %+v %v", u, err)
	}
	if _, err := ParseDSMUpdateCheck(`{"success":false,"error":{"code":105}}`); err == nil {
		t.Fatal("expected an error for a failed API call")
	}
	if ups, err := ParseSynopkgUpdates("[]\n"); err != nil || len(ups) != 0 {
		t.Fatalf("no package updates: %+v %v", ups, err)
	}
}

// Upgrade upgrades the packages, but never DSM itself.
func TestDSMUpgrade(t *testing.T) {
	upgraded := false
	f := (&runner.Fake{}).
		OnFunc("/usr/syno/bin/synopkg checkupdateall", func(runner.Cmd) runner.Result {
			if upgraded {
				return runner.Result{Stdout: "[]"}
			}
			return runner.Result{Stdout: synopkgPlex}
		}).
		OnFunc("/usr/syno/bin/synopkg upgradeall", func(runner.Cmd) runner.Result { upgraded = true; return runner.Result{} }).
		On("SYNO.Core.Upgrade.Server", runner.Result{Stdout: dsmNewer})
	res, err := (&DSM{r: f}).Upgrade(context.Background())
	if err != nil || !res.Changed || !strings.Contains(res.Summary, "upgraded 1 packages") || !strings.Contains(res.Summary, "install it in DSM") {
		t.Fatalf("got %+v %v", res, err)
	}
	for _, c := range f.Calls {
		if strings.Contains(c.String(), "synoupgrade") || strings.Contains(c.String(), "method=start") {
			t.Fatalf("DSM must not be upgraded: %v", c)
		}
	}

	// Nothing to upgrade: synopkg upgradeall is not run.
	f.Calls = nil
	if res, err := (&DSM{r: f}).Upgrade(context.Background()); err != nil || res.Changed || f.Ran("upgradeall") {
		t.Fatalf("up to date: %+v %v %v", res, err, f.Calls)
	}
}
