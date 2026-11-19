package app

import (
	"testing"

	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/provider"
	"github.com/bayuncao/oriel/internal/transport"
	"github.com/bayuncao/oriel/internal/transport/remote"
)

// D149、DV17：測試與本機構建沒有 orielhub_production 標記，所以有遠程。
func TestRemoteAvailableWithoutProductionTag(t *testing.T) {
	if !RemoteAvailable() {
		t.Fatal("a build without the orielhub_production tag has no remote")
	}
}

// D149、DV17：Options.Remote 為假時不建遠程（不讀鑰匙串、不連中繼），Close 照常；為真時建好並啟動，
// 經 remoteSecrets 用鑰匙串：沒有這一項是“還沒有”，鑰匙串不可用是 remote.keychain_unavailable。
func TestNewRemote(t *testing.T) {
	cases := []struct {
		name     string
		remote   bool
		keychain provider.Keychain
		// code 是打開遠程的結果；"" 是成功（沒有中繼地址，狀態 no_relay）
		code string
	}{
		{name: "production: no remote", remote: false},
		{name: "keychain works", remote: true, keychain: provider.NewMemKeychain()},
		{name: "no keychain", remote: true, code: string(remote.CodeKeychainUnavailable)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := New(ctxb2(), Options{DataDir: t.TempDir(), Inherited: proc.Env{}, Root: t.TempDir(),
				Remote: tc.remote, Keychain: tc.keychain})
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			rc := a.Remote()
			if !tc.remote {
				if rc != nil {
					t.Fatal("remote was created")
				}
				return
			}
			if rc == nil {
				t.Fatal("no remote")
			}
			if s := rc.Status(); s.On || s.State != remote.StateOff {
				t.Fatalf("status at start %+v", s)
			}
			err = rc.SetOn(true)
			if tc.code != "" {
				if w, _ := transport.Wire(err); err == nil || w.Code != tc.code {
					t.Fatalf("SetOn: %v, want %s", err, tc.code)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if s := rc.Status(); !s.On || s.State != remote.StateNoRelay {
				t.Fatalf("status after turning on %+v", s)
			}
		})
	}
}
