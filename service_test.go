// Copyright 2021 Canonical Ltd.

package jimm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/canonical/candid/candidtest"
	cofga "github.com/canonical/ofga"
	"github.com/coreos/go-oidc/v3/oidc"
	qt "github.com/frankban/quicktest"
	"github.com/go-macaroon-bakery/macaroon-bakery/v3/bakery"
	"github.com/go-macaroon-bakery/macaroon-bakery/v3/bakery/checkers"
	"github.com/go-macaroon-bakery/macaroon-bakery/v3/httpbakery"
	"github.com/go-macaroon-bakery/macaroon-bakery/v3/httpbakery/agent"
	"github.com/juju/juju/api"
	"github.com/juju/juju/api/client/cloud"
	jujucloud "github.com/juju/juju/cloud"
	"github.com/juju/juju/core/macaroon"
	"github.com/juju/names/v4"

	"github.com/canonical/jimm"
	"github.com/canonical/jimm/internal/dbmodel"
	"github.com/canonical/jimm/internal/jimmtest"
	"github.com/canonical/jimm/internal/openfga"
	ofganames "github.com/canonical/jimm/internal/openfga/names"
	"github.com/canonical/jimm/internal/vault"
)

func TestMain(m *testing.M) {
	code := m.Run()
	os.Exit(code)
}

func TestDefaultService(t *testing.T) {
	c := qt.New(t)

	_, _, cofgaParams, err := jimmtest.SetupTestOFGAClient(c.Name())
	c.Assert(err, qt.IsNil)
	svc, err := jimm.NewService(context.Background(), jimm.Params{
		DSN:                   jimmtest.CreateEmptyDatabase(c),
		OpenFGAParams:         cofgaParamsToJIMMOpenFGAParams(*cofgaParams),
		InsecureSecretStorage: true,
		OAuthAuthenticatorParams: jimm.OAuthAuthenticatorParams{
			IssuerURL:          "http://localhost:8082/realms/jimm",
			ClientID:           "jimm-device",
			Scopes:             []string{oidc.ScopeOpenID, "profile", "email"},
			SessionTokenExpiry: time.Duration(time.Hour),
		},
	})
	c.Assert(err, qt.IsNil)
	rr := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/debug/info", nil)
	c.Assert(err, qt.IsNil)
	svc.ServeHTTP(rr, req)
	resp := rr.Result()
	c.Check(resp.StatusCode, qt.Equals, http.StatusOK)
}

func TestServiceStartsWithoutSecretStore(t *testing.T) {
	c := qt.New(t)

	_, _, cofgaParams, err := jimmtest.SetupTestOFGAClient(c.Name())
	c.Assert(err, qt.IsNil)
	_, err = jimm.NewService(context.Background(), jimm.Params{
		DSN:           jimmtest.CreateEmptyDatabase(c),
		OpenFGAParams: cofgaParamsToJIMMOpenFGAParams(*cofgaParams),
		OAuthAuthenticatorParams: jimm.OAuthAuthenticatorParams{
			IssuerURL:          "http://localhost:8082/realms/jimm",
			ClientID:           "jimm-device",
			Scopes:             []string{oidc.ScopeOpenID, "profile", "email"},
			SessionTokenExpiry: time.Duration(time.Hour),
		},
	})
	c.Assert(err, qt.IsNil)
}

func TestAuthenticator(t *testing.T) {
	c := qt.New(t)

	_, _, cofgaParams, err := jimmtest.SetupTestOFGAClient(c.Name())
	c.Assert(err, qt.IsNil)

	p := jimm.Params{
		ControllerUUID:        "6acf4fd8-32d6-49ea-b4eb-dcb9d1590c11",
		DSN:                   jimmtest.CreateEmptyDatabase(c),
		ControllerAdmins:      []string{"admin"},
		OpenFGAParams:         cofgaParamsToJIMMOpenFGAParams(*cofgaParams),
		InsecureSecretStorage: true,
		OAuthAuthenticatorParams: jimm.OAuthAuthenticatorParams{
			IssuerURL:          "http://localhost:8082/realms/jimm",
			ClientID:           "jimm-device",
			Scopes:             []string{oidc.ScopeOpenID, "profile", "email"},
			SessionTokenExpiry: time.Duration(time.Hour),
		},
	}
	candid := startCandid(c, &p)
	svc, err := jimm.NewService(context.Background(), p)
	c.Assert(err, qt.IsNil)

	srv := httptest.NewTLSServer(svc)
	c.Cleanup(srv.Close)
	info := api.Info{
		Addrs: []string{srv.Listener.Addr().String()},
	}

	conn, err := api.Open(&info, api.DialOpts{
		BakeryClient:       userClient(candid, "alice", "admin"),
		InsecureSkipVerify: true,
	})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() {
		if err := conn.Close(); err != nil {
			c.Logf("closing connection: %s", err)
		}
	})

	c.Check(conn.ControllerTag(), qt.Equals, names.NewControllerTag("6acf4fd8-32d6-49ea-b4eb-dcb9d1590c11"))
	c.Check(conn.AuthTag(), qt.Equals, names.NewUserTag("alice@external"))
	c.Check(conn.ControllerAccess(), qt.Equals, "")

	conn, err = api.Open(&info, api.DialOpts{
		BakeryClient:       userClient(candid, "bob"),
		InsecureSkipVerify: true,
	})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() {
		if err := conn.Close(); err != nil {
			c.Logf("closing connection: %s", err)
		}
	})

	c.Check(conn.ControllerTag(), qt.Equals, names.NewControllerTag("6acf4fd8-32d6-49ea-b4eb-dcb9d1590c11"))
	c.Check(conn.AuthTag(), qt.Equals, names.NewUserTag("bob@external"))
	c.Check(conn.ControllerAccess(), qt.Equals, "")
}

const testVaultEnv = `clouds:
- name: test
  type: test
  regions:
  - name: test-region
`

func TestVault(t *testing.T) {
	c := qt.New(t)

	ofgaClient, _, cofgaParams, err := jimmtest.SetupTestOFGAClient(c.Name())
	c.Assert(err, qt.IsNil)

	p := jimm.Params{
		ControllerUUID:  "6acf4fd8-32d6-49ea-b4eb-dcb9d1590c11",
		DSN:             jimmtest.CreateEmptyDatabase(c),
		VaultAddress:    "http://localhost:8200",
		VaultAuthPath:   "/auth/approle/login",
		VaultPath:       "/jimm-kv/",
		VaultSecretFile: "./local/vault/approle.json",
		OpenFGAParams:   cofgaParamsToJIMMOpenFGAParams(*cofgaParams),
		OAuthAuthenticatorParams: jimm.OAuthAuthenticatorParams{
			IssuerURL:          "http://localhost:8082/realms/jimm",
			ClientID:           "jimm-device",
			Scopes:             []string{oidc.ScopeOpenID, "profile", "email"},
			SessionTokenExpiry: time.Duration(time.Hour),
		},
	}
	candid := startCandid(c, &p)
	vaultClient, _, creds, _ := jimmtest.VaultClient(c, ".")

	svc, err := jimm.NewService(context.Background(), p)
	c.Assert(err, qt.IsNil)

	env := jimmtest.ParseEnvironment(c, testVaultEnv)
	env.PopulateDBAndPermissions(c, names.NewControllerTag(p.ControllerUUID), svc.JIMM().Database, ofgaClient)

	srv := httptest.NewTLSServer(svc)
	c.Cleanup(srv.Close)
	info := api.Info{
		Addrs: []string{srv.Listener.Addr().String()},
	}

	conn, err := api.Open(&info, api.DialOpts{
		BakeryClient:       userClient(candid, "bob"),
		InsecureSkipVerify: true,
	})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() {
		if err := conn.Close(); err != nil {
			c.Logf("closing connection: %s", err)
		}
	})

	cloudClient := cloud.NewClient(conn)

	tag := names.NewCloudCredentialTag("test/bob@external/test-1").String()
	_, err = cloudClient.UpdateCloudsCredentials(map[string]jujucloud.Credential{
		tag: jujucloud.NewCredential(jujucloud.UserPassAuthType, map[string]string{
			"username": "test-user",
			"password": "test-secret",
		}),
	}, false)
	c.Assert(err, qt.IsNil)

	store := vault.VaultStore{
		Client:     vaultClient,
		AuthSecret: creds,
		AuthPath:   p.VaultAuthPath,
		KVPath:     p.VaultPath,
	}
	attr, err := store.Get(context.Background(), names.NewCloudCredentialTag("test/bob@external/test-1"))
	c.Assert(err, qt.IsNil)
	c.Check(attr, qt.DeepEquals, map[string]string{
		"username": "test-user",
		"password": "test-secret",
	})
}

func TestPostgresSecretStore(t *testing.T) {
	c := qt.New(t)

	_, _, cofgaParams, err := jimmtest.SetupTestOFGAClient(c.Name())
	c.Assert(err, qt.IsNil)

	p := jimm.Params{
		ControllerUUID:        "6acf4fd8-32d6-49ea-b4eb-dcb9d1590c11",
		DSN:                   jimmtest.CreateEmptyDatabase(c),
		OpenFGAParams:         cofgaParamsToJIMMOpenFGAParams(*cofgaParams),
		InsecureSecretStorage: true,
		OAuthAuthenticatorParams: jimm.OAuthAuthenticatorParams{
			IssuerURL:          "http://localhost:8082/realms/jimm",
			ClientID:           "jimm-device",
			Scopes:             []string{oidc.ScopeOpenID, "profile", "email"},
			SessionTokenExpiry: time.Duration(time.Hour),
		},
	}
	_, err = jimm.NewService(context.Background(), p)
	c.Assert(err, qt.IsNil)
}

func TestOpenFGA(t *testing.T) {
	c := qt.New(t)

	_, _, cofgaParams, err := jimmtest.SetupTestOFGAClient(c.Name())
	c.Assert(err, qt.IsNil)

	p := jimm.Params{
		ControllerUUID:   "6acf4fd8-32d6-49ea-b4eb-dcb9d1590c11",
		DSN:              jimmtest.CreateEmptyDatabase(c),
		OpenFGAParams:    cofgaParamsToJIMMOpenFGAParams(*cofgaParams),
		ControllerAdmins: []string{"alice", "eve"},
		OAuthAuthenticatorParams: jimm.OAuthAuthenticatorParams{
			IssuerURL:          "http://localhost:8082/realms/jimm",
			ClientID:           "jimm-device",
			Scopes:             []string{oidc.ScopeOpenID, "profile", "email"},
			SessionTokenExpiry: time.Duration(time.Hour),
		},
	}
	candid := startCandid(c, &p)
	svc, err := jimm.NewService(context.Background(), p)
	c.Assert(err, qt.IsNil)

	srv := httptest.NewTLSServer(svc)
	c.Cleanup(srv.Close)
	info := api.Info{
		Addrs: []string{srv.Listener.Addr().String()},
	}

	conn, err := api.Open(&info, api.DialOpts{
		BakeryClient:       userClient(candid, "bob"),
		InsecureSkipVerify: true,
	})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() {
		if err := conn.Close(); err != nil {
			c.Logf("closing connection: %s", err)
		}
	})

	client, err := jimm.NewOpenFGAClient(context.Background(), p.OpenFGAParams)
	c.Assert(err, qt.IsNil)

	// assert controller admins have been created in openfga
	for _, username := range []string{"alice", "eve"} {
		user := openfga.NewUser(
			&dbmodel.Identity{Name: username},
			client,
		)
		allowed, err := openfga.IsAdministrator(context.Background(), user, names.NewControllerTag(p.ControllerUUID))
		c.Assert(err, qt.IsNil)
		c.Assert(allowed, qt.IsTrue)
	}
}

func TestPublicKey(t *testing.T) {
	c := qt.New(t)

	_, _, cofgaParams, err := jimmtest.SetupTestOFGAClient(c.Name())
	c.Assert(err, qt.IsNil)

	p := jimm.Params{
		ControllerUUID:   "6acf4fd8-32d6-49ea-b4eb-dcb9d1590c11",
		DSN:              jimmtest.CreateEmptyDatabase(c),
		OpenFGAParams:    cofgaParamsToJIMMOpenFGAParams(*cofgaParams),
		ControllerAdmins: []string{"alice", "eve"},
		PrivateKey:       "c1VkV05+iWzCxMwMVcWbr0YJWQSEO62v+z3EQ2BhFMw=",
		PublicKey:        "pC8MEk9MS9S8fhyRnOJ4qARTcTAwoM9L1nH/Yq0MwWU=",
		OAuthAuthenticatorParams: jimm.OAuthAuthenticatorParams{
			IssuerURL:          "http://localhost:8082/realms/jimm",
			ClientID:           "jimm-device",
			Scopes:             []string{oidc.ScopeOpenID, "profile", "email"},
			SessionTokenExpiry: time.Duration(time.Hour),
		},
	}
	_ = startCandid(c, &p)
	svc, err := jimm.NewService(context.Background(), p)
	c.Assert(err, qt.IsNil)

	srv := httptest.NewTLSServer(svc)
	c.Cleanup(srv.Close)

	response, err := http.Get(srv.URL + "/macaroons/publickey")
	c.Assert(err, qt.IsNil)
	data, err := io.ReadAll(response.Body)
	c.Assert(err, qt.IsNil)
	c.Assert(string(data), qt.Equals, `{"PublicKey":"pC8MEk9MS9S8fhyRnOJ4qARTcTAwoM9L1nH/Yq0MwWU="}`)
}

func TestThirdPartyCaveatDischarge(t *testing.T) {
	c := qt.New(t)

	offer := dbmodel.ApplicationOffer{
		UUID: "7e4e7ffb-5116-4544-a400-f584d08c410e",
		Name: "test-application-offer",
	}
	user := dbmodel.Identity{
		Name: "alice@external",
	}

	ctx := context.Background()

	tests := []struct {
		about          string
		setup          func(c *qt.C, ofgaClient *openfga.OFGAClient, user *dbmodel.Identity)
		caveats        []string
		expectDeclared map[string]string
		expectedError  string
	}{{
		about:         "unknown caveats",
		caveats:       []string{"unknown-caveat"},
		expectedError: ".*third party refused discharge: cannot discharge: caveat not recognized",
	}, {
		about: "user is an offer reader",
		setup: func(c *qt.C, ofgaClient *openfga.OFGAClient, user *dbmodel.Identity) {
			u := openfga.NewUser(user, ofgaClient)
			err := u.SetApplicationOfferAccess(ctx, offer.ResourceTag(), ofganames.ReaderRelation)
			c.Assert(err, qt.IsNil)
		},
		caveats:       []string{fmt.Sprintf("is-consumer %s %s", user.ResourceTag(), offer.UUID)},
		expectedError: ".*cannot discharge: permission denied",
	}, {
		about:         "user is not an offer consumer",
		caveats:       []string{fmt.Sprintf("is-consumer %s %s", user.ResourceTag(), offer.UUID)},
		expectedError: ".*cannot discharge: permission denied",
	}, {
		about: "user is an offer consumer",
		setup: func(c *qt.C, ofgaClient *openfga.OFGAClient, user *dbmodel.Identity) {
			u := openfga.NewUser(user, ofgaClient)
			err := u.SetApplicationOfferAccess(ctx, offer.ResourceTag(), ofganames.ConsumerRelation)
			c.Assert(err, qt.IsNil)
		},
		caveats:        []string{fmt.Sprintf("is-consumer %s %s", user.ResourceTag(), offer.UUID)},
		expectDeclared: map[string]string{"offer-uuid": offer.UUID},
	}, {
		about: "user is an offer administrator",
		setup: func(c *qt.C, ofgaClient *openfga.OFGAClient, user *dbmodel.Identity) {
			u := openfga.NewUser(user, ofgaClient)
			err := u.SetApplicationOfferAccess(ctx, offer.ResourceTag(), ofganames.AdministratorRelation)
			c.Assert(err, qt.IsNil)
		},
		caveats:        []string{fmt.Sprintf("is-consumer %s %s", user.ResourceTag(), offer.UUID)},
		expectDeclared: map[string]string{"offer-uuid": offer.UUID},
	}}
	for _, test := range tests {
		c.Run(test.about, func(c *qt.C) {
			ofgaClient, _, cofgaParams, err := jimmtest.SetupTestOFGAClient(c.Name())
			c.Assert(err, qt.IsNil)

			p := jimm.Params{
				ControllerUUID:   "6acf4fd8-32d6-49ea-b4eb-dcb9d1590c11",
				DSN:              jimmtest.CreateEmptyDatabase(c),
				OpenFGAParams:    cofgaParamsToJIMMOpenFGAParams(*cofgaParams),
				ControllerAdmins: []string{"alice", "eve"},
				PrivateKey:       "c1VkV05+iWzCxMwMVcWbr0YJWQSEO62v+z3EQ2BhFMw=",
				PublicKey:        "pC8MEk9MS9S8fhyRnOJ4qARTcTAwoM9L1nH/Yq0MwWU=",
				OAuthAuthenticatorParams: jimm.OAuthAuthenticatorParams{
					IssuerURL:          "http://localhost:8082/realms/jimm",
					ClientID:           "jimm-device",
					Scopes:             []string{oidc.ScopeOpenID, "profile", "email"},
					SessionTokenExpiry: time.Duration(time.Hour),
				},
			}
			_ = startCandid(c, &p)
			svc, err := jimm.NewService(context.Background(), p)
			c.Assert(err, qt.IsNil)

			srv := httptest.NewTLSServer(svc)
			c.Cleanup(srv.Close)

			var pk bakery.PublicKey
			err = pk.UnmarshalText([]byte(p.PublicKey))
			c.Assert(err, qt.IsNil)

			locator := bakery.NewThirdPartyStore()
			locator.AddInfo(srv.URL+"/macaroons", bakery.ThirdPartyInfo{
				PublicKey: pk,
				Version:   bakery.LatestVersion,
			})

			if test.setup != nil {
				test.setup(c, ofgaClient, &user)
			}

			m, err := bakery.NewMacaroon(
				[]byte("root key"),
				[]byte("id"),
				"location",
				bakery.LatestVersion,
				macaroon.MacaroonNamespace,
			)
			c.Assert(err, qt.IsNil)

			kp := bakery.MustGenerateKey()

			for _, caveat := range test.caveats {
				err = m.AddCaveat(context.TODO(), checkers.Caveat{
					Location:  srv.URL + "/macaroons",
					Condition: caveat,
				}, kp, locator)
				c.Assert(err, qt.IsNil)
			}

			bakeryClient := httpbakery.NewClient()
			ms, err := bakeryClient.DischargeAll(context.TODO(), m)
			if test.expectedError != "" {
				c.Assert(err, qt.ErrorMatches, test.expectedError)
			} else {
				c.Assert(err, qt.IsNil)
				c.Assert(ms, qt.HasLen, 2)

				declaredCaveats := checkers.InferDeclared(macaroon.MacaroonNamespace, ms)
				c.Logf("declared caveats %v", declaredCaveats)
				c.Assert(declaredCaveats, qt.DeepEquals, test.expectDeclared)
			}
		})
	}
}

func startCandid(c *qt.C, p *jimm.Params) *candidtest.Server {
	candid := candidtest.NewServer()
	c.Cleanup(candid.Close)
	p.CandidURL = candid.URL.String()

	tpi, err := httpbakery.ThirdPartyInfoForLocation(context.Background(), nil, candid.URL.String())
	c.Assert(err, qt.IsNil)
	pk, err := tpi.PublicKey.MarshalText()
	c.Assert(err, qt.IsNil)
	p.CandidPublicKey = string(pk)

	candid.AddUser("jimm-agent", candidtest.GroupListGroup)
	buf, err := json.Marshal(agent.AuthInfo{
		Key: key(candid, "jimm-agent"),
		Agents: []agent.Agent{{
			URL:      candid.URL.String(),
			Username: "jimm-agent",
		}},
	})
	c.Assert(err, qt.IsNil)
	p.BakeryAgentFile = filepath.Join(c.TempDir(), "agent.json")
	err = os.WriteFile(p.BakeryAgentFile, buf, 0400)
	c.Assert(err, qt.IsNil)
	return candid
}

func userClient(candid *candidtest.Server, user string, groups ...string) *httpbakery.Client {
	candid.AddUser(user, groups...)
	client := httpbakery.NewClient()
	agent.SetUpAuth(client, &agent.AuthInfo{
		Key: key(candid, user),
		Agents: []agent.Agent{{
			URL:      candid.URL.String(),
			Username: user,
		}},
	})
	return client
}

func key(candid *candidtest.Server, user string) *bakery.KeyPair {
	key := candid.UserPublicKey(user)
	return &bakery.KeyPair{
		Public:  bakery.PublicKey{Key: bakery.Key(key.Public.Key)},
		Private: bakery.PrivateKey{Key: bakery.Key(key.Private.Key)},
	}
}

// cofgaParamsToJIMMOpenFGAParams To avoid circular references, the test setup function (jimmtest.SetupTestOFGAClient)
// does not provide us with an instance of `jimm.OpenFGAParams`, so it just returns a `cofga.OpenFGAParams` instance.
// This method reshapes the later into the former.
func cofgaParamsToJIMMOpenFGAParams(cofgaParams cofga.OpenFGAParams) jimm.OpenFGAParams {
	return jimm.OpenFGAParams{
		Scheme:    cofgaParams.Scheme,
		Host:      cofgaParams.Host,
		Port:      cofgaParams.Port,
		Store:     cofgaParams.StoreID,
		Token:     cofgaParams.Token,
		AuthModel: cofgaParams.AuthModelID,
	}
}
