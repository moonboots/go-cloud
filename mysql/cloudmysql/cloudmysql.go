// Copyright 2018 The Go Cloud Development Kit Authors
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

// Package cloudmysql provides connections to managed MySQL Cloud SQL instances.
// See https://cloud.google.com/sql/docs/mysql/ for more information.
//
// URLs
//
// For mysql.Open, cloudmysql registers for the scheme "cloudmysql".
// The default URL opener will create a connection using the default
// credentials from the environment, as described in
// https://cloud.google.com/docs/authentication/production.
// To customize the URL opener, or for more details on the URL format,
// see URLOpener.
//
// See https://gocloud.dev/concepts/urls/ for background information.
package cloudmysql // import "gocloud.dev/mysql/cloudmysql"

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"contrib.go.opencensus.io/integrations/ocsql"
	"github.com/GoogleCloudPlatform/cloudsql-proxy/proxy/proxy"
	"github.com/go-sql-driver/mysql"
	"gocloud.dev/gcp"
	"gocloud.dev/gcp/cloudsql"
	cdkmysql "gocloud.dev/mysql"
)

// Scheme is the URL scheme cloudmysql registers its URLOpener under on
// mysql.DefaultMux.
const Scheme = "cloudmysql"

func init() {
	cdkmysql.DefaultURLMux().RegisterMySQL(Scheme, new(lazyCredsOpener))
}

// lazyCredsOpener obtains Application Default Credentials on the first call
// to OpenMySQLURL.
type lazyCredsOpener struct {
	init   sync.Once
	opener *URLOpener
	err    error
}

func (o *lazyCredsOpener) OpenMySQLURL(ctx context.Context, u *url.URL) (*sql.DB, error) {
	o.init.Do(func() {
		creds, err := gcp.DefaultCredentials(ctx)
		if err != nil {
			o.err = err
			return
		}
		client, err := gcp.NewHTTPClient(gcp.DefaultTransport(), creds.TokenSource)
		if err != nil {
			o.err = err
			return
		}
		certSource := cloudsql.NewCertSource(client)
		o.opener = &URLOpener{CertSource: certSource}
	})
	if o.err != nil {
		return nil, fmt.Errorf("cloudmysql open %v: %v", u, o.err)
	}
	return o.opener.OpenMySQLURL(ctx, u)
}

// URLOpener opens Cloud MySQL URLs like
// "cloudmysql://user:password@project/region/instance/dbname".
type URLOpener struct {
	// CertSource specifies how the opener will obtain authentication information.
	// CertSource must not be nil.
	CertSource proxy.CertSource

	// TraceOpts contains options for OpenCensus.
	TraceOpts []ocsql.TraceOption
}

// OpenMySQLURL opens a new GCP database connection wrapped with OpenCensus instrumentation.
func (uo *URLOpener) OpenMySQLURL(ctx context.Context, u *url.URL) (*sql.DB, error) {
	if uo.CertSource == nil {
		return nil, fmt.Errorf("cloudmysql: URLOpener CertSource is nil")
	}
	instance, dbName, err := instanceFromURL(u)
	if err != nil {
		return nil, fmt.Errorf("cloudmysql: open %v: %v", u, err)
	}
	// TODO(light): Avoid global registry once https://github.com/go-sql-driver/mysql/issues/771 is fixed.
	dialerCounter.mu.Lock()
	dialerNum := dialerCounter.n
	dialerCounter.mu.Unlock()
	client := &proxy.Client{
		Port:  3307,
		Certs: uo.CertSource,
	}
	dialerName := fmt.Sprintf("gocloud.dev/mysql/gcpmysql/%d", dialerNum)
	mysql.RegisterDial(dialerName, client.Dial)

	password, _ := u.User.Password()
	cfg := &mysql.Config{
		AllowNativePasswords: true,
		Net:                  dialerName,
		Addr:                 instance,
		User:                 u.User.Username(),
		Passwd:               password,
		DBName:               dbName,
	}
	db := sql.OpenDB(connector{cfg.FormatDSN(), uo.TraceOpts})
	return db, nil
}

func instanceFromURL(u *url.URL) (instance, db string, _ error) {
	path := u.Host + u.Path // everything after scheme but before query or fragment
	parts := strings.SplitN(path, "/", 4)
	if len(parts) < 4 {
		return "", "", fmt.Errorf("%s is not in the form project/region/instance/dbname", path)
	}
	for _, part := range parts {
		if part == "" {
			return "", "", fmt.Errorf("%s is not in the form project/region/instance/dbname", path)
		}
	}
	return parts[0] + ":" + parts[1] + ":" + parts[2], parts[3], nil
}

var dialerCounter struct {
	mu sync.Mutex
	n  int
}

type connector struct {
	dsn       string
	traceOpts []ocsql.TraceOption
}

func (c connector) Connect(ctx context.Context) (driver.Conn, error) {
	return c.Driver().Open(c.dsn)
}

func (c connector) Driver() driver.Driver {
	return ocsql.Wrap(mysql.MySQLDriver{}, c.traceOpts...)
}
