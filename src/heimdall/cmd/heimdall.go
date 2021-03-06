// Copyright © 2018 Playground Global, LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"crypto/rand"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/base64"
	"fmt"
	"image/png"
	"io/ioutil"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/pquerna/otp/totp"

	"playground/ca"
	"playground/config"
	"playground/httputil"
	"playground/log"
)

/*
 * Configuration types & helpers
 */

type serverConfig struct {
	Debug                    bool
	Port                     int
	BindAddress              string
	LogFile                  string
	SQLiteDBFile             string
	SelfSignedClientCertFile string
	ServerCertFile           string
	ServerKeyFile            string
	CACertFile               string
	CAKeyFile                string
	CAKeyPassword            string
	TLSAuthFile              string
	OVPNTemplateFile         string
	APIHeader                string
	APISecret                string
}

var cfg = &serverConfig{
	false,
	9090,
	"127.0.0.1",
	"./heimdall.log",
	"./heimdall.sqlite3",
	"./client.crt",
	"./server.crt",
	"./server.key",
	"./ca.crt",
	"./ca.key",
	"Sekr1tPassw0rd!",
	"./tls-auth.pem",
	"./template.ovpn",
	"X-Heimdall-Secret",
	"Sekr1tPassw0rd",
}

func initConfig(cfg *serverConfig) {
	config.Load(cfg)

	if cfg.LogFile != "" {
		log.SetLogFile(cfg.LogFile)
	}
	if config.Debug || cfg.Debug {
		log.SetLogLevel(log.LEVEL_DEBUG)
	}
}

/*
 * Main loop which starts the HTTP server & defines handlers
 */
func main() {
	initConfig(cfg)

	server, mux := httputil.NewHardenedServer(cfg.BindAddress, cfg.Port)
	server.RequireClientRoot(cfg.SelfSignedClientCertFile)
	w := httputil.Wrapper().WithPanicHandler().WithSecretSentry(cfg.APIHeader, cfg.APISecret)
	mux.HandleFunc("/users", w.WithMethodSentry("GET").Wrap(usersHandler))
	mux.HandleFunc("/user/", w.WithMethodSentry("GET", "PUT", "DELETE").Wrap(userHandler))
	mux.HandleFunc("/certs", w.WithMethodSentry("GET").Wrap(certsHandler))
	mux.HandleFunc("/certs/", w.WithMethodSentry("GET", "POST").Wrap(certsHandler))
	mux.HandleFunc("/cert/", w.WithMethodSentry("GET", "DELETE").Wrap(certHandler))
	mux.HandleFunc("/events", w.WithMethodSentry("GET", "DELETE").Wrap(eventsHandler))
	mux.HandleFunc("/settings", w.WithMethodSentry("GET", "PUT").Wrap(settingsHandler))
	mux.HandleFunc("/whitelist", w.WithMethodSentry("GET").Wrap(whitelistHandler))
	mux.HandleFunc("/whitelist/", w.WithMethodSentry("DELETE", "PUT").Wrap(whitelistHandler))

	mux.HandleFunc("/", w.WithMethodSentry("GET").Wrap(func(writer http.ResponseWriter, req *http.Request) {
		// serve a 404 to all other requests; note that "/" is effectively a wildcard
		log.Warn("server", "incoming unknown request to '"+req.URL.Path+"'")
		httputil.SendJSON(writer, http.StatusNotFound, struct{}{})
	}))

	log.Status("server.http", "starting HTTP on port "+strconv.Itoa(cfg.Port))
	log.Error("server.http", "shutting down; error?", server.ListenAndServeTLS(cfg.ServerCertFile, cfg.ServerKeyFile))
}

/*
 * Package-local utilities
 */

func extractSegment(path string, n int) string {
	chunks := strings.Split(path, "/")
	if len(chunks) > n {
		return chunks[n]
	}
	return ""
}

// Database access helpers
func getDB() *sql.DB {
	cxn, err := sql.Open("sqlite3", cfg.SQLiteDBFile)
	if err != nil {
		panic(err)
	}
	return cxn
}

func writeDatabaseByQuery(query string, params ...interface{}) {
	cxn := getDB()
	defer cxn.Close()

	_, err := cxn.Exec(query, params...)
	if err != nil {
		panic(err)
	}
}

// function & type to load settings from DB (generally needed fresh for each request, so not
// cacheable)
type settings struct {
	ServiceName                     string
	ClientLimit, IssuedCertDuration int
	WhitelistedDomains              []string
	WhitelistedUsers                []string `json:",omitEmpty"`
}

func loadSettings() *settings {
	cxn := getDB()
	defer cxn.Close()

	ret := &settings{"Bifröst VPN", 2, 90, []string{}, []string{}}

	if rows, err := cxn.Query("select key, value from settings"); err != nil {
		panic(err)
	} else {
		defer rows.Close()
		var k, v string
		for rows.Next() {
			rows.Scan(&k, &v)
			switch k {
			case "ServiceName":
				ret.ServiceName = v
			case "ClientLimit":
				if tmp, err := strconv.ParseInt(v, 10, 32); err == nil {
					ret.ClientLimit = int(tmp)
				} else {
					panic(err)
				}
			case "IssuedCertDuration":
				if tmp, err := strconv.ParseInt(v, 10, 32); err == nil {
					ret.IssuedCertDuration = int(tmp)
				} else {
					panic(err)
				}
			case "WhitelistedDomains":
				for _, d := range strings.Split(v, " ") {
					if d != "" {
						ret.WhitelistedDomains = append(ret.WhitelistedDomains, d)
					}
				}
				sort.Strings(ret.WhitelistedDomains)
			default:
			}
		}
	}
	if rows, err := cxn.Query("select email from whitelist order by email"); err != nil {
		panic(err)
	} else {
		defer rows.Close()
		var email string
		for rows.Next() {
			rows.Scan(&email)
			if email != "" {
				ret.WhitelistedUsers = append(ret.WhitelistedUsers, email)
			}
		}
	}
	return ret
}

func storeSettings(s *settings) {
	writeDatabaseByQuery("insert or replace into settings (key, value) values (?, ?)", "ServiceName", s.ServiceName)
	writeDatabaseByQuery("insert or replace into settings (key, value) values (?, ?)", "IssuedCertDuration", s.IssuedCertDuration)
	writeDatabaseByQuery("insert or replace into settings (key, value) values (?, ?)", "ClientLimit", s.ClientLimit)
	writeDatabaseByQuery("insert or replace into settings (key, value) values (?, ?)", "WhitelistedDomains", strings.Join(s.WhitelistedDomains, " "))
}

// makeCertSerial generates a random string suitable for use as the serial number string in a
// certificate. Note that this is random so collisions can technically occur; however the
// infrastructure uses (or is assumed to use) fingerprints for things like revocations, rather than
// serial numbers
func makeCertSerial() string {
	ceiling := new(big.Int).Lsh(big.NewInt(1), 128)
	newSerial, err := rand.Int(rand.Reader, ceiling)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", newSerial)
}

/*
 * API endpoint handlers
 */

func usersHandler(writer http.ResponseWriter, req *http.Request) {
	// GET /users -- fetch all known users
	//   I: None
	//   O: {Users: [{Email: "", ActiveCerts: 0, RevokedCerts: 0}]}
	//	 200: results
	// Non-GET: 405 (method not allowed)

	type user struct {
		Email        string
		ActiveCerts  int
		RevokedCerts int
	}
	users := []user{}

	q := "select t.email, count(distinct c.fingerprint), count(distinct c2.fingerprint) from totp as t left join certs as c on t.email=c.email and c.revoked is null left join certs as c2 on t.email=c2.email and c2.revoked is not null group by t.email"
	cxn := getDB()
	defer cxn.Close()
	if rows, err := cxn.Query(q); err != nil {
		panic(err)
	} else {
		defer rows.Close()
		for rows.Next() {
			u := user{}
			rows.Scan(&u.Email, &u.ActiveCerts, &u.RevokedCerts)
			users = append(users, u)
		}
	}
	sort.Slice(users, func(i, j int) bool { return users[i].Email < users[j].Email })

	httputil.SendJSON(writer, http.StatusOK, &struct{ Users []user }{users})
}

func userHandler(writer http.ResponseWriter, req *http.Request) {
	// GET /user/<email> -- fetch a list of user's certs
	//   I: None
	//   O: {Email: "", Created: "", ActiveCerts: [<cert>], RevokedCerts: [<cert>]}
	//   200: the object requested; 404: Email not known
	//   <cert>: {Fingerprint: "", Created: "", Expires: "", Revoked: "", Description: ""}
	// PUT /user/<email> -- (re)generate a user's TOTP seed, creating user if necessary
	//   I: None
	//   O: {Email: "", TOTPURL: ""}
	//   200: exists and TOTP reset; 201 (created): new user created & TOTP set
	// DELETE /user/<email> -- delete a user's TOTP seed and revoke all certs
	//   I: None
	//   O: {RevokedCerts: [<cert>]}    (<cert> is as above)
	//   200: deleted/revoked; 404: email not found
	//   RevokedCerts can be empty if user had no certs
	// Non-GET/PUT/DELETE -- 405 (method not allowed): can't edit whitelists

	TAG := "userHandler"

	email := extractSegment(req.URL.Path, 2)
	if email == "" {
		log.Error(TAG, fmt.Sprintf("bad path '%s'", req.URL.Path))
		httputil.SendJSON(writer, http.StatusBadRequest, struct{}{})
		return
	}

	type cert struct {
		Fingerprint, Created, Expires, Revoked, Description string
	}

	switch req.Method {
	case "GET":
		type user struct {
			Email, Created            string
			ActiveCerts, RevokedCerts []*cert
		}

		cxn := getDB()
		defer cxn.Close()
		u := &user{Email: email, ActiveCerts: []*cert{}, RevokedCerts: []*cert{}}
		q := "select created from totp where email=?"
		if rows, err := cxn.Query(q, u.Email); err != nil {
			panic(err)
		} else {
			defer rows.Close()
			if !rows.Next() {
				log.Status(TAG, "request for nonexistent user", u.Email)
				httputil.SendJSON(writer, http.StatusNotFound, struct{}{})
				return
			}
			rows.Scan(&(u.Created))
			if rows.Next() {
				log.Error(TAG, "multiple database entries for user", u.Email)
				httputil.SendJSON(writer, http.StatusInternalServerError, struct{}{})
				return
			}
		}
		q = "select fingerprint, created, expires, desc, revoked from certs where email=?"
		if rows, err := cxn.Query(q, u.Email); err != nil {
			panic(err)
		} else {
			defer rows.Close()
			for rows.Next() {
				c := &cert{}
				rows.Scan(&c.Fingerprint, &c.Created, &c.Expires, &c.Description, &c.Revoked)
				if c.Revoked == "" {
					u.ActiveCerts = append(u.ActiveCerts, c)
				} else {
					u.RevokedCerts = append(u.RevokedCerts, c)
				}
			}
			sort.Slice(u.ActiveCerts, func(i, j int) bool { return u.ActiveCerts[i].Description < u.ActiveCerts[j].Description })
			sort.Slice(u.RevokedCerts, func(i, j int) bool { return u.RevokedCerts[i].Description < u.RevokedCerts[j].Description })
		}

		httputil.SendJSON(writer, http.StatusOK, &u)

	case "PUT":
		type res struct {
			Email, TOTPURL string
		}

		settings := loadSettings()
		key, err := totp.Generate(totp.GenerateOpts{
			Issuer:      settings.ServiceName,
			AccountName: email,
		})
		if err != nil {
			panic(err)
		}

		q := "insert or replace into totp (email, seed, updated) values (?, ?, datetime('now'))"
		writeDatabaseByQuery(q, email, key.Secret(), email)

		// record the event
		q = "insert into events (event, email, value) values (?, ?, ?)"
		writeDatabaseByQuery(q, "TOTP set", email, "")

		var buf bytes.Buffer
		img, err := key.Image(200, 200)
		if err != nil {
			panic(err)
		}
		png.Encode(&buf, img)
		imageURL := base64.StdEncoding.EncodeToString(buf.Bytes())
		imageURL = fmt.Sprintf("data:image/png;base64,%s", imageURL)

		log.Status(TAG, fmt.Sprintf("generated TOTP seed for '%s'", email))
		httputil.SendJSON(writer, http.StatusOK, &res{email, imageURL})

	case "DELETE":
		fps := []string{}
		q := "select fingerprint from certs where email=?"
		cxn := getDB()
		defer cxn.Close()
		if rows, err := cxn.Query(q, email); err != nil {
			panic(err)
		} else {
			defer rows.Close()
			for rows.Next() {
				var fp string
				rows.Scan(&fp)
				fps = append(fps, fp)
			}
		}
		if len(fps) > 0 {
			writeDatabaseByQuery("update certs set revoked=datetime('now') where email=?", email)
		}
		writeDatabaseByQuery("delete from totp where email=?", email)

		// record the event
		q = "insert into events (event, email, value) values (?, ?, ?)"
		writeDatabaseByQuery(q, "user deleted", email, fmt.Sprintf("%d certs revoked", len(fps)))

		log.Status(TAG, fmt.Sprintf("cleared TOTP seed (deleted user) for '%s'", email))
		httputil.SendJSON(writer, http.StatusOK, &struct{ RevokedCerts []string }{fps})

	default:
		panic("API method sentinel misconfiguration")
	}
}

func certsHandler(writer http.ResponseWriter, req *http.Request) {
	// GET /certs -- get all certs for all users
	//   I: None
	//   O: {Certs: [{Email: "", Created: "", ActiveCerts: [<cert>], RevokedCerts: [<cert>]}]}
	//   200: the object above
	// GET /certs/<email> -- get a list of certs for the indicated user
	//   I: none
	//   O: {Email: "", Created: "", ActiveCerts: [<cert>], RevokedCerts: [<cert>]}
	//   200: the object requested; 404: email not found
	//   Note: if email has no TOTP but does have certs, Created is ""
	// POST /certs/<email> -- create a certificate for the indicated user
	//   I: {Email: "", Description: ""}
	//   O: {OVPNDataURL: ""} // Note: represented as the base64-encoded value of a data: href
	//   201: created; 400 (bad request): missing email or description;
	//   401 (unauthorized): user is already at cert limit
	// Non-GET: 409 (bad method)

	TAG := "/certs/"

	email := extractSegment(req.URL.Path, 2)

	type cert struct {
		Fingerprint, Created, Expires, Revoked, Description string
	}

	switch req.Method {
	case "GET":
		if email == "" { // i.e. /certs or /certs/ -- means fetch all users
			type user struct {
				Email, Created            string
				ActiveCerts, RevokedCerts []*cert
			}
			users := make(map[string]*user)
			q := "select t.email, t.created, c.fingerprint, c.created, c.expires, c.revoked, c.desc from totp as t, certs as c where t.email=c.email"
			// note that this query skips certs that have no extant user; WAI
			cxn := getDB()
			defer cxn.Close()
			if rows, err := cxn.Query(q); err != nil {
				panic(err)
			} else {
				defer rows.Close()
				for rows.Next() {
					var email, created string
					c := &cert{}
					rows.Scan(&email, &created, &c.Fingerprint, &c.Created, &c.Expires, &c.Revoked, &c.Description)
					var u *user
					if u, ok := users[email]; !ok {
						u = &user{Email: email}
						users[email] = u
						u.ActiveCerts = []*cert{}
						u.RevokedCerts = []*cert{}
						u.Created = created
					}
					if c.Revoked != "" {
						u.ActiveCerts = append(u.ActiveCerts, c)
					} else {
						u.RevokedCerts = append(u.RevokedCerts, c)
					}
				}
				res := struct{ Certs []*user }{[]*user{}}
				for _, u := range users {
					sort.Slice(u.ActiveCerts, func(i, j int) bool { return u.ActiveCerts[i].Description < u.ActiveCerts[j].Description })
					sort.Slice(u.RevokedCerts, func(i, j int) bool { return u.RevokedCerts[i].Description < u.RevokedCerts[j].Description })
					res.Certs = append(res.Certs, u)
				}
				sort.Slice(res.Certs, func(i, j int) bool { return res.Certs[i].Email < res.Certs[j].Email })
				httputil.SendJSON(writer, http.StatusOK, &res)
				return
			}
		} else { // i.e. /certs/<something> -- means fetch a particular user
			q := "select t.created, c.fingerprint, c.created, c.expires, c.desc, c.revoked from totp as t left join certs as c on t.email=c.email where t.email=?"
			cxn := getDB()
			defer cxn.Close()
			if rows, err := cxn.Query(q, email); err != nil {
				panic(err)
			} else {
				defer rows.Close()
				res := struct {
					Email, Created            string
					ActiveCerts, RevokedCerts []cert
				}{Email: email, ActiveCerts: []cert{}, RevokedCerts: []cert{}}
				for rows.Next() {
					c := cert{}
					rows.Scan(&res.Created, &c.Fingerprint, &c.Created, &c.Expires, &c.Description, &c.Revoked)
					if c.Fingerprint == "" {
						// can happen if the user has TOTP and no certs, as a consequence of the left join; avoiding putting it in response
						continue
					}
					if c.Revoked == "" {
						res.ActiveCerts = append(res.ActiveCerts, c)
					} else {
						res.RevokedCerts = append(res.RevokedCerts, c)
					}
				}
				if res.Created == "" { // database can't not have this, so must mean no results
					log.Debug(TAG, "request for nonexistent user", email)
					httputil.SendJSON(writer, http.StatusNotFound, struct{}{})
					return
				}
				sort.Slice(res.ActiveCerts, func(i, j int) bool { return res.ActiveCerts[i].Description < res.ActiveCerts[j].Description })
				sort.Slice(res.RevokedCerts, func(i, j int) bool { return res.RevokedCerts[i].Description < res.RevokedCerts[j].Description })
				httputil.SendJSON(writer, http.StatusOK, &res)
				return
			}
		}
	case "POST":
		if email == "" {
			log.Warn(TAG, "missing user on POST", req.URL.Path)
			httputil.SendJSON(writer, http.StatusBadRequest, struct{}{})
			return
		}

		reqBody := &struct{ Email, Description string }{}
		if err := httputil.PopulateFromBody(reqBody, req); err != nil {
			log.Warn(TAG, "missing or malformed request JSON", req.URL.Path)
			httputil.SendJSON(writer, http.StatusBadRequest, struct{}{})
			return
		}

		if email != reqBody.Email {
			log.Warn(TAG, "mismatched URL/JSON request", req.URL.Path, email, reqBody.Email)
			httputil.SendJSON(writer, http.StatusBadRequest, struct{}{})
			return
		}
		if reqBody.Description == "" {
			log.Warn(TAG, "JSON request missing description", req.URL.Path)
			httputil.SendJSON(writer, http.StatusBadRequest, struct{}{})
			return
		}

		var err error
		var key, crt, cacrt, tlsauth []byte // various keymatter to be embedded in the .ovpn file
		var fp string
		var t *template.Template // .ovpn template
		var ovpn bytes.Buffer
		var rows *sql.Rows

		// check that user exists
		q := "select email from totp where email=?"
		cxn := getDB()
		defer cxn.Close()
		if rows, err = cxn.Query(q, email); err != nil {
			panic(err)
		}
		defer rows.Close()
		if !rows.Next() {
			// can't issue a cert for an unrecorded user
			log.Warn(TAG, "attempt to issue cert for nonexistent user", email, q)
			httputil.SendJSON(writer, http.StatusNotFound, struct{}{})
			return
		}
		if rows.Next() {
			// shouldn't be possible, if database constraints are correct
			panic("multiple users returned by database")
		}

		// generate a serial number for the new cert
		serial := &big.Int{}
		if _, ok := serial.SetString(makeCertSerial(), 16); !ok {
			panic("unable to create serial number for new cert")
		}

		// load up the CA signing cert & keys
		authority := &ca.Authority{}
		if err = authority.LoadFromPEM(cfg.CACertFile, cfg.CAKeyFile, cfg.CAKeyPassword); err != nil {
			panic(err)
		}

		s := loadSettings()

		// generate a signed cert & private key (never written to disk)
		subject := &pkix.Name{
			Organization: []string{s.ServiceName},
			CommonName:   email,
		}
		var kp *ca.Keypair
		if kp, err = authority.CreateClientKeypair(s.IssuedCertDuration, subject, serial, 4096); err != nil {

			panic(err)
		}

		if fp, err = kp.CertFingerprint(); err != nil {
			panic(err)
		}

		// gather all the keymatter in PEM
		if crt, key, err = kp.ToPEM("", false); err != nil { // client cert & key
			panic(err)
		}
		if tlsauth, err = ioutil.ReadFile(cfg.TLSAuthFile); err != nil { // tls-auth shared secret
			panic(err)
		}
		cacrt = authority.ExportCertChain() // CA cert

		// construct the .ovpn from template
		if t, err = template.ParseFiles(cfg.OVPNTemplateFile); err != nil {
			panic(err)
		}
		if err = t.Execute(&ovpn, struct{ CA, Cert, Key, TLSAuth string }{string(cacrt), string(crt), string(key), string(tlsauth)}); err != nil {
			panic(err)
		}

		// save a record of the cert to the database
		q = fmt.Sprintf("insert into certs (email, fingerprint, desc, expires) values (?, ?, ?, date('now','+%d day'))", s.IssuedCertDuration)
		writeDatabaseByQuery(q, email, fp, reqBody.Description)

		// record the event
		q = "insert into events (event, email, value) values (?, ?, ?)"
		writeDatabaseByQuery(q, "certificate issued", email, fmt.Sprintf("%s - %s", fp, reqBody.Description))

		// transmit to client
		log.Status(TAG, fmt.Sprintf("issued new certificate '%s' for '%s'", fp, email))

		dataURL := base64.StdEncoding.EncodeToString(ovpn.Bytes())
		dataURL = fmt.Sprintf("data:image/ovpn;base64,%s", dataURL)
		httputil.SendJSON(writer, http.StatusCreated, struct{ OVPNDataURL string }{dataURL})
	default:
		panic("API method sentinel misconfiguration")
	}
}

func certHandler(writer http.ResponseWriter, req *http.Request) {
	// GET /cert/<fingerprint> -- fetch details for the indicated cert
	//   I: None
	//   O: {Email: "", Fingerprint: "", Created: "", Expires: "", Revoked: "", Description: ""}
	//   200: the object above; 404: no such fingerprint
	// DELETE /cert/<fingerprint> -- revoke the indicated cert
	//   I: None
	//   O: {Email: "", Fingerprint: "", Created: "", Expires: "", Revoked: "", Description: ""}
	//   200: the cert was revoked; 404: no such fingerprint; 400: malformed fingerprint
	// Non-GET/DELETE: 409 (bad method)

	TAG := "/cert/"

	fp := extractSegment(req.URL.Path, 2)
	if fp == "" {
		log.Warn(TAG, "missing fingerprint")
		httputil.SendJSON(writer, http.StatusBadRequest, struct{}{})
		return
	}

	switch req.Method {
	case "GET":
		q := "select email, fingerprint, created, expires, revoked, desc from certs where fingerprint=?"
		cxn := getDB()
		defer cxn.Close()
		if rows, err := cxn.Query(q, fp); err != nil {
			panic(err)
		} else {
			defer rows.Close()
			if !rows.Next() {
				log.Warn(TAG, "request for nonexistent fingerprint", fp)
				httputil.SendJSON(writer, http.StatusNotFound, struct{}{})
				return
			}
			res := struct{ Email, Fingerprint, Created, Expires, Revoked, Description string }{}
			rows.Scan(&res.Email, &res.Fingerprint, &res.Created, &res.Expires, &res.Revoked, &res.Description)
			if rows.Next() {
				log.Error(TAG, "multiple results for fingerprint", fp)
				httputil.SendJSON(writer, http.StatusInternalServerError, struct{}{})
				return
			}
			httputil.SendJSON(writer, http.StatusOK, &res)
		}

	case "DELETE":
		var email string
		q := "select email from certs where fingerprint=?"
		cxn := getDB()
		defer cxn.Close()
		if rows, err := cxn.Query(q, fp); err != nil {
			panic(err)
		} else {
			if !rows.Next() {
				log.Warn(TAG, "attempt to revoke nonexistent cert", fp)
				httputil.SendJSON(writer, http.StatusOK, struct{}{})
				return
			}
			rows.Scan(&email)
			rows.Close() // must manually close so that writes below work
		}
		//cxn.Close()
		q = "update certs set revoked=datetime('now') where fingerprint=?"
		writeDatabaseByQuery(q, fp)

		// record the event
		q = "insert into events (event, email, value) values (?, ?, ?)"
		writeDatabaseByQuery(q, "certificate revoked", email, fp)

		log.Status(TAG, fmt.Sprintf("revoked certificate '%s'", fp))
		httputil.SendJSON(writer, http.StatusOK, struct{}{})

	default:
		panic("API method sentinel misconfiguration")
	}
}

func eventsHandler(writer http.ResponseWriter, req *http.Request) {
	// GET /events -- fetch events log
	//   I: None
	//   O: {Events: [{Event: "", Email: "", Value: "", Timestamp: ""}]}
	//   200: the object above
	// DELETE /events -- clear the log (e.g. as part of log extraction/rotation)
	//   I: None
	//   O: {Events: [{Event: "", Email: "", Value: "", Timestamp: ""}]}
	//   200: the object above + the log was cleared
	// Non-GET/DELETE: 409 (bad method)
	// Accepts a GET query parameter of "?before=" for pagination. Unless the value of this parameter
	// is "all", it returns at most 25 results

	TAG := "/events"

	type event struct{ Event, Email, Value, Timestamp string }
	events := []*event{}

	if err := req.ParseForm(); err != nil {
		panic(err)
	}
	before := req.FormValue("before")

	cxn := getDB()
	defer cxn.Close()
	var rows *sql.Rows
	var err error
	if before == "" {
		q := "select event, email, value, ts from events order by ts desc limit 25"
		rows, err = cxn.Query(q)
	} else {
		if before == "all" {
			q := "select event, email, value, ts from events order by ts desc"
			rows, err = cxn.Query(q)
		} else {
			t, err := time.Parse("2006-01-02T15:04:05Z", before)
			if err != nil {
				httputil.SendJSON(writer, http.StatusBadRequest, struct{}{})
				return
			}
			before = t.Format("2006-01-02 15:04:05")
			q := "select event, email, value, ts from events where ts < ? order by ts desc limit 25"
			rows, err = cxn.Query(q, before)
		}
	}
	if err != nil {
		panic(err)
	} else {
		defer rows.Close()
		for rows.Next() {
			ev := &event{}
			rows.Scan(&ev.Event, &ev.Email, &ev.Value, &ev.Timestamp)
			events = append(events, ev)
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[j].Timestamp < events[i].Timestamp })

	httputil.SendJSON(writer, http.StatusOK, struct{ Events []*event }{events})

	if req.Method == "DELETE" {
		log.Status(TAG, "clearing event log")
		writeDatabaseByQuery("delete from events")
		writeDatabaseByQuery("insert into events (event, email, value) values (?, ?, ?)", "events log reset", "", fmt.Sprintf("%d events cleared", len(events)))
		log.Status(TAG, "cleared event log")
	}
}

func settingsHandler(writer http.ResponseWriter, req *http.Request) {
	// GET /settings -- fetch service metadata
	//   I: None
	//   O: {ServiceName: "", ClientLimit: 2, IssuedCertDuration: 90, WhitelistedDomains:[""]}
	//   200: the object above
	// PUT /settings -- update service metadata
	//   I: {ServiceName: "", ClientLimit: 2, IssuedCertDuration: 90, WhitelistedDomains:[""]}
	//   O: {ServiceName: "", ClientLimit: 2, IssuedCertDuration: 90, WhitelistedDomains:[""]}
	//   200: the object above + values stored; 400 (bad request): missing or malformed values, or empty body
	// Non-GET/DELETE: 409 (bad method)

	TAG := "/settings"
	switch req.Method {
	case "GET":
		httputil.SendJSON(writer, http.StatusOK, loadSettings())
	case "PUT":
		s := settings{}
		if err := httputil.PopulateFromBody(&s, req); err != nil {
			log.Error(TAG, "error parsing request body", req.Method)
			httputil.SendJSON(writer, http.StatusBadRequest, struct{}{})
		}
		storeSettings(&s)
		httputil.SendJSON(writer, http.StatusOK, loadSettings())
	default:
		panic("API method sentinel misconfiguration")
	}
}

func whitelistHandler(writer http.ResponseWriter, req *http.Request) {
	// GET /whitelist -- fetch list of whitelisted users
	//   I: None
	//   O: {Users: [""]}
	//   200: the object above
	// PUT /whitelist/<email> -- add a user to the whitelist
	//   I: None
	//   O: {Users: [""]}
	//   200: new complete list of users; 400: malformed or missing email
	//   Idempotent if user is already whitelisted.
	// DELETE /whitelist/<email> -- delete a user to the whitelist
	//   I: None
	//   O: {Users: [""]}
	//   200: new complete list of users; 404: user not whitelisted; 400: malformed or missing email
	// Non-GET/DELETE: 409 (bad method)
	// Returned list of users is sorted.

	TAG := "whitelistHandler"

	email := extractSegment(req.URL.Path, 2)

	switch req.Method {
	case "GET":
		if email != "" {
			httputil.SendJSON(writer, http.StatusBadRequest, struct{}{})
			return
		}
		q := "select email from whitelist order by email"
		cxn := getDB()
		defer cxn.Close()
		emails := []string{}
		if rows, err := cxn.Query(q); err != nil {
			panic(err)
		} else {
			defer rows.Close()
			for rows.Next() {
				rows.Scan(&email)
				emails = append(emails, email)
			}
		}
		httputil.SendJSON(writer, http.StatusOK, struct{ Users []string }{emails})
	case "PUT":
		if email == "" {
			httputil.SendJSON(writer, http.StatusBadRequest, struct{}{})
			return
		}
		writeDatabaseByQuery("insert or replace into whitelist (email) values (?)", email)
		log.Status(TAG, fmt.Sprintf("added '%s' to user whitelist", email))
		httputil.SendJSON(writer, http.StatusOK, struct{ Users []string }{loadSettings().WhitelistedUsers})
	case "DELETE":
		if email == "" {
			httputil.SendJSON(writer, http.StatusBadRequest, struct{}{})
			return
		}
		writeDatabaseByQuery("delete from whitelist where email=?", email)
		log.Status(TAG, fmt.Sprintf("deleted '%s' from user whitelist", email))
		httputil.SendJSON(writer, http.StatusOK, struct{ Users []string }{loadSettings().WhitelistedUsers})
	default:
		panic("API method sentinel misconfiguration")
	}
}
