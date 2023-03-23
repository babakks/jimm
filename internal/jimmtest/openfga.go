package jimmtest

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/jackc/pgx/v4"
	"github.com/oklog/ulid/v2"
	openfga "github.com/openfga/go-sdk"
	"github.com/openfga/go-sdk/credentials"

	"github.com/CanonicalLtd/jimm/internal/errors"
	ofga "github.com/CanonicalLtd/jimm/internal/openfga"
)

var (
	authDefinitions    = []openfga.TypeDefinition{}
	calcDefinitionOnce sync.Once

	setups   map[string]testSetup
	setupsMu sync.Mutex
)

func init() {
	setups = make(map[string]testSetup)
}

type testSetup struct {
	client     openfga.OpenFgaApi
	ofgaClient *ofga.OFGAClient
	config     *openfga.Configuration
}

func getAuthModelDefinition() (_ []openfga.TypeDefinition, err error) {
	calcDefinitionOnce.Do(func() {
		desiredFolder := "local"
		authPath := ""
		var pwd string
		pwd, err = os.Getwd()
		if err != nil {
			return
		}
		for ok := true; ok; {
			if pwd == "/" {
				break
			}
			files, err := ioutil.ReadDir(pwd)
			if err != nil {
				return
			}

			for _, f := range files {
				if f.Name() == desiredFolder {
					ok = true
					authPath = pwd
				}
			}
			// Move up a directory
			pwd = filepath.Dir(pwd)
		}
		if authPath == "" {
			err = fmt.Errorf("auth path is empty")
			return
		}

		var b []byte
		b, err = os.ReadFile(path.Join(authPath, "/local/openfga/authorisation_model.json"))
		if err != nil {
			return
		}
		wrapper := make(map[string]interface{})
		err = json.Unmarshal(b, &wrapper)
		if err != nil {
			return
		}

		b, err = json.Marshal(wrapper["type_definitions"])
		if err != nil {
			return
		}

		err = json.Unmarshal(b, &authDefinitions)
		if err != nil {
			return
		}
	})

	return authDefinitions, err

}

// SetupTestOFGAClient is intended to be used per test, in that it
// creates a store based on the current tests name and is expected to
// be a single use instance (due to the initial removing of a store).
//
// The benefit of not cleaning up the store immediately afterwards,
// enables the debugging of created tuples in test development.
func SetupTestOFGAClient(names ...string) (openfga.OpenFgaApi, *ofga.OFGAClient, *openfga.Configuration, error) {
	ctx := context.Background()

	testName := strings.ReplaceAll(strings.Join(names, "_"), " ", "_")

	setupsMu.Lock()
	defer setupsMu.Unlock()
	setup, ok := setups[testName]
	if ok {
		return setup.client, setup.ofgaClient, setup.config, nil
	}

	openFGATestConfig := openfga.Configuration{
		ApiScheme: "http",
		ApiHost:   "localhost:8080",
		Credentials: &credentials.Credentials{
			Method: credentials.CredentialsMethodApiToken,
			Config: &credentials.Config{
				ApiToken: "jimm",
			},
		},
	}
	err := RemoveStore(ctx, testName)
	if err != nil {
		return nil, nil, nil, err
	}

	uuid := ulid.Make().String()
	err = CreateStore(ctx, testName, uuid)
	if err != nil {
		return nil, nil, nil, err
	}

	cfg, err := openfga.NewConfiguration(openFGATestConfig)
	if err != nil {
		return nil, nil, nil, err
	}

	client := openfga.NewAPIClient(cfg)

	client.SetStoreId(uuid)
	api := client.OpenFgaApi

	typeDefinitions, err := getAuthModelDefinition()
	ar := openfga.NewWriteAuthorizationModelRequest(typeDefinitions)
	if err != nil {
		return nil, nil, nil, err
	}

	amr, _, err := api.WriteAuthorizationModel(ctx).Body(*ar).Execute()
	if err != nil {
		return nil, nil, nil, err
	}

	wrapperClient := ofga.NewOpenFGAClient(client.OpenFgaApi, amr.GetAuthorizationModelId())
	cfg.StoreId = uuid

	setups[testName] = testSetup{
		client:     client.OpenFgaApi,
		ofgaClient: wrapperClient,
		config:     cfg,
	}
	return client.OpenFgaApi, wrapperClient, cfg, nil
}

// RemoveStore removes an OpenFGA store (via db) by NAME.
// Currently, OpenFGA does not support this as it is expected to remove a store by ID.
//
// However, in a testing scenario, we want a simple and quick solution to cleanup
// a store per test,
func RemoveStore(ctx context.Context, name string) error {
	conn, err := pgx.Connect(context.Background(), "postgresql://jimm:jimm@localhost/jimm")
	if err != nil {
		return errors.E(err)
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, fmt.Sprintf("DELETE FROM store WHERE name = '%s';", name))
	if err != nil {
		return errors.E(err)
	}
	return nil
}

// CreateStore adds a store to OpenFGA (via db), circumventing the superficial rules
// set in their server around character limit (64).
func CreateStore(ctx context.Context, name string, id string) error {
	conn, err := pgx.Connect(context.Background(), "postgresql://jimm:jimm@localhost/jimm")
	if err != nil {
		return errors.E(err)
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(
		ctx,
		fmt.Sprintf("INSERT INTO store(id, name, created_at, updated_at) VALUES('%s', '%s', '%s', '%s');",
			id,
			name,
			"2023-01-18 12:14:45.048376+00",
			"2023-01-18 12:14:45.048376+00",
		),
	)
	if err != nil {
		return errors.E(err)
	}
	return nil
}

// TruncateOpenFgaTuples truncates the tuple and changelog tables used by openFGA.
func TruncateOpenFgaTuples(ctx context.Context) error {
	conn, err := pgx.Connect(context.Background(), "postgresql://jimm:jimm@localhost/jimm")
	if err != nil {
		return errors.E(err)
	}
	defer conn.Close(ctx)
	conn.Exec(ctx, "TRUNCATE TABLE tuple;")
	conn.Exec(ctx, "TRUNCATE TABLE changelog;")
	return nil
}