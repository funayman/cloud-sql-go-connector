// Copyright 2021 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cloudsqlconn

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/cloudsqlconn/errtype"
	"cloud.google.com/go/cloudsqlconn/internal/cloudsql"
	"cloud.google.com/go/cloudsqlconn/internal/mock"
	"golang.org/x/oauth2"
)

func testSuccessfulDial(ctx context.Context, t *testing.T, d *Dialer, i string, opts ...DialOption) {
	conn, err := d.Dial(ctx, i, opts...)
	if err != nil {
		t.Fatalf("expected Dial to succeed, but got error: %v", err)
	}
	defer func() { _ = conn.Close() }()

	data, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("expected ReadAll to succeed, got error %v", err)
	}
	if string(data) != "my-instance" {
		t.Fatalf("expected known response from the server, but got %v", string(data))
	}
}

func TestDialerCanConnectToInstance(t *testing.T) {
	inst := mock.NewFakeCSQLInstance("my-project", "my-region", "my-instance")
	svc, cleanup, err := mock.NewSQLAdminService(
		context.Background(),
		mock.InstanceGetSuccess(inst, 1),
		mock.CreateEphemeralSuccess(inst, 1),
	)
	if err != nil {
		t.Fatalf("failed to init SQLAdminService: %v", err)
	}
	stop := mock.StartServerProxy(t, inst)
	defer func() {
		stop()
		if err := cleanup(); err != nil {
			t.Fatalf("%v", err)
		}
	}()

	d, err := NewDialer(context.Background(),
		WithDefaultDialOptions(WithPublicIP()),
		WithTokenSource(mock.EmptyTokenSource{}),
	)
	if err != nil {
		t.Fatalf("expected NewDialer to succeed, but got error: %v", err)
	}
	d.sqladmin = svc

	testSuccessfulDial(context.Background(), t, d, "my-project:my-region:my-instance", WithPublicIP())
}

func TestDialWithAdminAPIErrors(t *testing.T) {
	inst := mock.NewFakeCSQLInstance("my-project", "my-region", "my-instance")
	svc, cleanup, err := mock.NewSQLAdminService(context.Background())
	if err != nil {
		t.Fatalf("failed to init SQLAdminService: %v", err)
	}
	stop := mock.StartServerProxy(t, inst)
	defer func() {
		stop()
		_ = cleanup()
	}()

	d, err := NewDialer(context.Background(),
		WithDefaultDialOptions(WithPublicIP()),
		WithTokenSource(mock.EmptyTokenSource{}),
	)
	if err != nil {
		t.Fatalf("expected NewDialer to succeed, but got error: %v", err)
	}
	d.sqladmin = svc

	_, err = d.Dial(context.Background(), "my-project:my-region:my-instance")
	var wantErr *errtype.RefreshError
	if !errors.As(err, &wantErr) {
		t.Fatalf("when API call fails, want = %T, got = %v", wantErr, err)
	}
}

func TestDialWithConfigurationErrors(t *testing.T) {
	inst := mock.NewFakeCSQLInstance("my-project", "my-region", "my-instance",
		mock.WithCertExpiry(time.Now().Add(-time.Hour)))

	svc, cleanup, err := mock.NewSQLAdminService(
		context.Background(),
		mock.InstanceGetSuccess(inst, 3),
		mock.CreateEphemeralSuccess(inst, 3),
	)
	if err != nil {
		t.Fatalf("failed to init SQLAdminService: %v", err)
	}
	defer func() { _ = cleanup() }()

	d, err := NewDialer(context.Background(),
		WithDefaultDialOptions(WithPublicIP()),
		WithTokenSource(mock.EmptyTokenSource{}),
		// give refresh plenty of time to complete in slower CI builds
		WithRefreshTimeout(time.Minute),
	)
	if err != nil {
		t.Fatalf("expected NewDialer to succeed, but got error: %v", err)
	}
	d.sqladmin = svc

	_, err = d.Dial(context.Background(), "my-project:my-region:my-instance", WithPrivateIP())
	if err == nil {
		t.Fatal("when IP type is invalid, want = error, got = nil")
	}

	_, err = d.Dial(context.Background(), "my-project:my-region:my-instance")
	if err == nil {
		t.Fatal("when server proxy socket is unavailable, want = error, got = nil")
	}

	stop := mock.StartServerProxy(t, inst)
	defer stop()

	_, err = d.Dial(context.Background(), "my-project:my-region:my-instance")
	if err == nil {
		t.Fatal("when TLS handshake fails, want = error, got = nil")
	}
}

var fakeServiceAccount = []byte(`{
  "type": "service_account",
  "project_id": "a-project-id",
  "private_key_id": "a-private-key-id",
  "private_key": "a-private-key",
  "client_email": "email@example.com",
  "client_id": "12345",
  "auth_uri": "https://accounts.google.com/o/oauth2/auth",
  "token_uri": "https://oauth2.googleapis.com/token",
  "auth_provider_x509_cert_url": "https://www.googleapis.com/oauth2/v1/certs",
  "client_x509_cert_url": "https://www.googleapis.com/robot/v1/metadata/x509/email%40example.com"
}`)

func TestIAMAuthn(t *testing.T) {
	tcs := []struct {
		desc         string
		opts         Option
		wantIAMAuthN bool
	}{
		{
			desc:         "When Credentials are provided with IAM Authn ENABLED",
			opts:         WithOptions(WithIAMAuthN(), WithCredentialsJSON(fakeServiceAccount)),
			wantIAMAuthN: true,
		},
		{
			desc:         "When Credentials are provided with IAM Authn DISABLED",
			opts:         WithCredentialsJSON(fakeServiceAccount),
			wantIAMAuthN: false,
		},
	}

	for _, tc := range tcs {
		t.Run(tc.desc, func(t *testing.T) {
			d, err := NewDialer(context.Background(), tc.opts)
			if err != nil {
				t.Fatalf("NewDialer failed with error = %v", err)
			}
			if gotIAMAuthN := d.defaultDialCfg.refreshCfg.UseIAMAuthN; gotIAMAuthN != tc.wantIAMAuthN {
				t.Fatalf("want = %v, got = %v", tc.wantIAMAuthN, gotIAMAuthN)
			}
		})
	}
}

func TestIAMAuthNErrors(t *testing.T) {
	tcs := []struct {
		desc    string
		version string
		opts    Option
	}{
		{
			desc:    "when the database engine is SQL Server",
			version: "SQLSERVER",
			opts:    WithIAMAuthN(),
		},
	}

	for _, tc := range tcs {
		t.Run(tc.desc, func(t *testing.T) {
			inst := mock.NewFakeCSQLInstance("proj", "region", "inst",
				mock.WithEngineVersion(tc.version),
			)
			svc, cleanup, err := mock.NewSQLAdminService(
				context.Background(),
				mock.InstanceGetSuccess(inst, 1),
				mock.CreateEphemeralSuccess(inst, 1),
			)
			if err != nil {
				t.Fatalf("mock.NewSQLAdminService(): %v", err)
			}
			defer func() { _ = cleanup() }()

			stop := mock.StartServerProxy(t, inst)
			defer stop()

			d, err := NewDialer(context.Background(),
				WithIAMAuthNTokenSources(
					mock.EmptyTokenSource{},
					mock.EmptyTokenSource{},
				), tc.opts)
			if err != nil {
				t.Fatalf("NewDialer failed with error = %v", err)
			}
			d.sqladmin = svc

			_, err = d.Dial(context.Background(), "proj:region:inst")
			t.Log(err)
			if err == nil {
				t.Fatalf("version = %v, want error, got nil", tc.version)
			}
		})
	}
}

func TestDialerWithCustomDialFunc(t *testing.T) {
	inst := mock.NewFakeCSQLInstance("my-project", "my-region", "my-instance")
	svc, cleanup, err := mock.NewSQLAdminService(
		context.Background(),
		mock.InstanceGetSuccess(inst, 1),
		mock.CreateEphemeralSuccess(inst, 1),
	)
	if err != nil {
		t.Fatalf("failed to init SQLAdminService: %v", err)
	}
	d, err := NewDialer(context.Background(),
		WithTokenSource(mock.EmptyTokenSource{}),
		WithDialFunc(func(ctx context.Context, network, addr string) (net.Conn, error) {
			return nil, errors.New("sentinel error")
		}),
	)
	if err != nil {
		t.Fatalf("expected NewDialer to succeed, but got error: %v", err)
	}
	d.sqladmin = svc
	defer func() {
		if err := cleanup(); err != nil {
			t.Fatalf("%v", err)
		}
	}()

	_, err = d.Dial(context.Background(), "my-project:my-region:my-instance")
	if !strings.Contains(err.Error(), "sentinel error") {
		t.Fatalf("want = sentinel error, got = %v", err)
	}
}

func TestDialerEngineVersion(t *testing.T) {
	tests := []string{
		"MYSQL_5_7", "POSTGRES_14", "SQLSERVER_2019_STANDARD", "MYSQL_8_0_18",
	}
	for _, wantEV := range tests {
		t.Run(wantEV, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			inst := mock.NewFakeCSQLInstance(
				"my-project", "my-region", "my-instance",
				mock.WithEngineVersion(wantEV))
			svc, cleanup, err := mock.NewSQLAdminService(
				ctx,
				mock.InstanceGetSuccess(inst, 1),
				mock.CreateEphemeralSuccess(inst, 1),
			)
			if err != nil {
				t.Fatalf("failed to init SQLAdminService: %v", err)
			}
			d, err := NewDialer(context.Background(),
				WithTokenSource(mock.EmptyTokenSource{}),
			)
			if err != nil {
				t.Fatalf("failed to init Dialer: %v", err)
			}
			d.sqladmin = svc
			defer func() {
				if err := cleanup(); err != nil {
					t.Fatalf("%v", err)
				}
			}()

			gotEV, err := d.EngineVersion(ctx, "my-project:my-region:my-instance")
			if err != nil {
				t.Fatalf("failed to retrieve engine version: %v", err)
			}
			if wantEV != gotEV {
				t.Errorf("InstanceEngineVersion(%s) failed: want %v, got %v", wantEV, gotEV, err)
			}
		})
	}
}

func TestDialerUserAgent(t *testing.T) {
	data, err := os.ReadFile("version.txt")
	if err != nil {
		t.Fatalf("failed to read version.txt: %v", err)
	}
	ver := strings.TrimSpace(string(data))
	want := "cloud-sql-go-connector/" + ver
	if want != userAgent {
		t.Errorf("embed version mismatched: want %q, got %q", want, userAgent)
	}
}

func TestWarmup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	inst := mock.NewFakeCSQLInstance("my-project", "my-region", "my-instance")
	stop := mock.StartServerProxy(t, inst)
	defer stop()
	tests := []struct {
		desc          string
		warmupOpts    []DialOption
		dialOpts      []DialOption
		expectedCalls []*mock.Request
	}{
		{
			desc:       "warmup and dial are the same",
			warmupOpts: []DialOption{WithDialIAMAuthN(true)},
			dialOpts:   []DialOption{WithDialIAMAuthN(true)},
			expectedCalls: []*mock.Request{
				mock.InstanceGetSuccess(inst, 1),
				mock.CreateEphemeralSuccess(inst, 1),
			},
		},
		{
			desc:       "warmup and dial are different",
			warmupOpts: []DialOption{WithDialIAMAuthN(true)},
			dialOpts:   []DialOption{WithDialIAMAuthN(false)},
			expectedCalls: []*mock.Request{
				mock.InstanceGetSuccess(inst, 2),
				mock.CreateEphemeralSuccess(inst, 2),
			},
		},
		{
			desc:       "warmup and default dial are different",
			warmupOpts: []DialOption{WithDialIAMAuthN(true)},
			dialOpts:   []DialOption{},
			expectedCalls: []*mock.Request{
				mock.InstanceGetSuccess(inst, 2),
				mock.CreateEphemeralSuccess(inst, 2),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			svc, cleanup, err := mock.NewSQLAdminService(ctx, test.expectedCalls...)
			if err != nil {
				t.Fatalf("failed to init SQLAdminService: %v", err)
			}
			d, err := NewDialer(context.Background(), WithTokenSource(mock.EmptyTokenSource{}))
			if err != nil {
				t.Fatalf("failed to init Dialer: %v", err)
			}
			d.sqladmin = svc
			defer func() {
				if err := cleanup(); err != nil {
					t.Fatalf("%v", err)
				}
			}()

			// Warmup once with the "default" options
			err = d.Warmup(ctx, "my-project:my-region:my-instance", test.warmupOpts...)
			if err != nil {
				t.Fatalf("Warmup failed: %v", err)
			}
			// Call EngineVersion to make sure we block until both API calls are completed.
			_, err = d.EngineVersion(ctx, "my-project:my-region:my-instance")
			if err != nil {
				t.Fatalf("Warmup failed: %v", err)
			}
			// Dial once with the "dial" options
			testSuccessfulDial(ctx, t, d, "my-project:my-region:my-instance", test.dialOpts...)
		})
	}
}

func TestDialDialerOptsConflicts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	inst := mock.NewFakeCSQLInstance("my-project", "my-region", "my-instance")
	stop := mock.StartServerProxy(t, inst)
	defer stop()
	tests := []struct {
		desc          string
		dialerOpts    []Option
		dialOpts      []DialOption
		expectedCalls []*mock.Request
	}{
		{
			desc:       "dialer opts set and dial uses default",
			dialerOpts: []Option{WithIAMAuthN()},
			dialOpts:   []DialOption{},
			expectedCalls: []*mock.Request{
				mock.InstanceGetSuccess(inst, 1),
				mock.CreateEphemeralSuccess(inst, 1),
			},
		},
		{
			desc:       "dialer and dial opts are the same",
			dialerOpts: []Option{WithIAMAuthN()},
			dialOpts:   []DialOption{WithDialIAMAuthN(true)},
			expectedCalls: []*mock.Request{
				mock.InstanceGetSuccess(inst, 1),
				mock.CreateEphemeralSuccess(inst, 1),
			},
		},
		{
			desc:       "dialer and dial opts are different",
			dialerOpts: []Option{WithIAMAuthN()},
			dialOpts:   []DialOption{WithDialIAMAuthN(false)},
			expectedCalls: []*mock.Request{
				mock.InstanceGetSuccess(inst, 2),
				mock.CreateEphemeralSuccess(inst, 2),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			svc, cleanup, err := mock.NewSQLAdminService(ctx, test.expectedCalls...)
			if err != nil {
				t.Fatalf("failed to init SQLAdminService: %v", err)
			}
			d, err := NewDialer(
				context.Background(),
				WithIAMAuthNTokenSources(mock.EmptyTokenSource{}, mock.EmptyTokenSource{}),
				WithOptions(test.dialerOpts...),
			)
			if err != nil {
				t.Fatalf("failed to init Dialer: %v", err)
			}
			d.sqladmin = svc
			defer func() {
				if err := cleanup(); err != nil {
					t.Fatalf("%v", err)
				}
			}()

			// Dial once with the "default" options
			testSuccessfulDial(ctx, t, d, "my-project:my-region:my-instance")

			// Dial once with the "dial" options
			testSuccessfulDial(ctx, t, d, "my-project:my-region:my-instance", test.dialOpts...)
		})
	}
}

func TestTokenSourceWithIAMAuthN(t *testing.T) {
	ts := oauth2.StaticTokenSource(&oauth2.Token{})
	tcs := []struct {
		desc    string
		opts    []Option
		wantErr bool
	}{
		{
			desc:    "when token source is set with IAM AuthN",
			opts:    []Option{WithTokenSource(ts), WithIAMAuthN()},
			wantErr: true,
		},
		{
			desc:    "when IAM AuthN token source is set without IAM AuthN",
			opts:    []Option{WithIAMAuthNTokenSources(ts, ts)},
			wantErr: true,
		},
	}
	for _, tc := range tcs {
		t.Run(tc.desc, func(t *testing.T) {
			_, err := NewDialer(context.Background(), tc.opts...)
			gotErr := err != nil
			if tc.wantErr != gotErr {
				t.Fatalf("err: want = %v, got = %v", tc.wantErr, gotErr)
			}
		})
	}
}

func TestDialerRemovesInvalidInstancesFromCache(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping slow unit test")
	}
	// When a dialer attempts to retrieve connection info for a
	// non-existent instance, it should delete the instance from
	// the cache and ensure no background refresh happens (which would be
	// wasted cycles).
	good := mock.NewFakeCSQLInstance("my-project", "my-region", "my-instance")
	svc, cleanup, err := mock.NewSQLAdminService(context.Background())
	if err != nil {
		t.Fatalf("failed to init SQLAdminService: %v", err)
	}
	stop := mock.StartServerProxy(t, good)
	defer func() {
		stop()
		if err := cleanup(); err != nil {
			t.Fatalf("%v", err)
		}
	}()

	d, err := NewDialer(context.Background(),
		WithDefaultDialOptions(WithPublicIP()),
		WithTokenSource(mock.EmptyTokenSource{}),
	)
	if err != nil {
		t.Fatalf("expected NewDialer to succeed, but got error: %v", err)
	}
	d.sqladmin = svc
	defer func(d *Dialer) {
		err := d.Close()
		if err != nil {
			t.Log(err)
		}
	}(d)

	badInstanceConnectionName := "doesntexist:us-central1:doesntexist"
	_, _ = d.Dial(context.Background(), badInstanceConnectionName)

	// The internal cache is not revealed publicly, so check the internal cache
	// to confirm the instance is no longer present.
	badCN, _ := cloudsql.ParseConnName(badInstanceConnectionName)
	d.lock.RLock()
	_, ok := d.instances[badCN]
	d.lock.RUnlock()
	if ok {
		t.Fatal("bad instance was not removed from the cache")
	}
}

func TestDialerSupportsOneOffDialFunction(t *testing.T) {
	ctx := context.Background()
	inst := mock.NewFakeCSQLInstance("p", "r", "i")
	svc, cleanup, err := mock.NewSQLAdminService(
		context.Background(),
		mock.InstanceGetSuccess(inst, 1),
		mock.CreateEphemeralSuccess(inst, 1),
	)
	if err != nil {
		t.Fatalf("failed to init SQLAdminService: %v", err)
	}
	d, err := NewDialer(ctx, WithTokenSource(mock.EmptyTokenSource{}))
	if err != nil {
		t.Fatal(err)
	}
	d.sqladmin = svc
	defer func() {
		if err := d.Close(); err != nil {
			t.Log(err)
		}
		_ = cleanup()
	}()

	sentinelErr := errors.New("dial func was called")
	f := func(context.Context, string, string) (net.Conn, error) {
		return nil, sentinelErr
	}

	if _, err := d.Dial(ctx, "p:r:i", WithOneOffDialFunc(f)); !errors.Is(err, sentinelErr) {
		t.Fatal("one-off dial func was not called")
	}
}
