/*
 * Copyright © 2015-2018 Aeneas Rekkas <aeneas+oss@aeneas.io>
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * @author		Aeneas Rekkas <aeneas+oss@aeneas.io>
 * @Copyright 	2017-2018 Aeneas Rekkas <aeneas+oss@aeneas.io>
 * @license 	Apache-2.0
 */

package consent

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/ory/go-convenience/stringsx"
	"github.com/ory/hydra/client"
	"github.com/pkg/errors"
	"github.com/rubenv/sql-migrate"
)

var migrations = &migrate.MemoryMigrationSource{
	Migrations: []*migrate.Migration{
		{
			Id: "1",
			Up: []string{
				`CREATE TABLE hydra_oauth2_consent_request (
	challenge  			varchar(40) NOT NULL PRIMARY KEY,
	verifier 			varchar(40) NOT NULL,
	client_id			varchar(255) NOT NULL,
	subject				varchar(255) NOT NULL,
	request_url			text NOT NULL,
	skip				bool NOT NULL,
	requested_scope		text NOT NULL,
	csrf				varchar(40) NOT NULL,
	authenticated_at	timestamp NULL,
	requested_at  		timestamp NOT NULL DEFAULT now(),
	oidc_context		text NOT NULL
)`,
				// It would probably make sense here to have a FK relation to clients, but it increases testing complexity and might also
				// purge important audit data when a client is deleted. Also, stale data does not have a negative impact here
				// 		FOREIGN KEY (client_id) REFERENCES hydra_client (id) ON DELETE CASCADE
				`CREATE TABLE hydra_oauth2_authentication_request (
	challenge  			varchar(40) NOT NULL PRIMARY KEY,
	requested_scope		text NOT NULL,
	verifier 			varchar(40) NOT NULL,
	csrf				varchar(40) NOT NULL,
	subject				varchar(255) NOT NULL,
	request_url			text NOT NULL,
	skip				bool NOT NULL,
	client_id			varchar(255) NOT NULL,
	requested_at  		timestamp NOT NULL DEFAULT now(),
	authenticated_at	timestamp NULL,
	oidc_context		text NOT NULL
)`,
				// It would probably make sense here to have a FK relation to clients, but it increases testing complexity and might also
				// purge important audit data when a client is deleted. Also, stale data does not have a negative impact here
				// 		FOREIGN KEY (client_id) REFERENCES hydra_client (id) ON DELETE CASCADE
				`CREATE TABLE hydra_oauth2_authentication_session (
	id      			varchar(40) NOT NULL PRIMARY KEY,
	authenticated_at  	timestamp NOT NULL DEFAULT NOW(),
	subject 			varchar(255) NOT NULL
)`,
				`CREATE TABLE hydra_oauth2_consent_request_handled (
	challenge  				varchar(40) NOT NULL PRIMARY KEY,
	granted_scope			text NOT NULL,
	remember				bool NOT NULL,
	remember_for			int NOT NULL,
	error					text NOT NULL,
	requested_at  			timestamp NOT NULL DEFAULT now(),
	session_access_token 	text NOT NULL,
	session_id_token 		text NOT NULL,
	authenticated_at		timestamp NULL,
	was_used 				bool NOT NULL
)`,
				`CREATE TABLE hydra_oauth2_authentication_request_handled (
	challenge  			varchar(40) NOT NULL PRIMARY KEY,
	subject 			varchar(255) NOT NULL,
	remember			bool NOT NULL,
	remember_for		int NOT NULL,
	error				text NOT NULL,
	acr					text NOT NULL,
	requested_at  		timestamp NOT NULL DEFAULT now(),
	authenticated_at	timestamp NULL,
	was_used 			bool NOT NULL
)`,
			},
			Down: []string{
				"DROP TABLE hydra_oauth2_consent_request",
				"DROP TABLE hydra_oauth2_authentication_request",
				"DROP TABLE hydra_oauth2_authentication_session",
				"DROP TABLE hydra_oauth2_consent_request_handled",
				"DROP TABLE hydra_oauth2_authentication_request_handled",
			},
		},
		{
			Id: "2",
			Up: []string{
				`ALTER TABLE hydra_oauth2_consent_request ADD forced_subject_identifier VARCHAR(255) NULL DEFAULT ''`,
				`ALTER TABLE hydra_oauth2_authentication_request_handled ADD forced_subject_identifier VARCHAR(255) NULL DEFAULT ''`,
				`CREATE TABLE hydra_oauth2_obfuscated_authentication_session (
	subject  			varchar(255) NOT NULL,
	client_id 			varchar(255) NOT NULL,
	subject_obfuscated	varchar(255) NOT NULL,
	PRIMARY KEY(subject, client_id)
)`,
			},
			Down: []string{
				`ALTER TABLE hydra_oauth2_consent_request DROP COLUMN forced_subject_identifier`,
				`ALTER TABLE hydra_oauth2_authentication_request_handled DROP COLUMN forced_subject_identifier`,
				"DROP TABLE hydra_oauth2_obfuscated_authentication_session",
			},
		},
	},
}

var sqlParamsAuthenticationRequestHandled = []string{
	"challenge",
	"subject",
	"remember",
	"remember_for",
	"error",
	"requested_at",
	"authenticated_at",
	"acr",
	"was_used",
	"forced_subject_identifier",
}

var sqlParamsAuthenticationRequest = []string{
	"challenge",
	"verifier",
	"client_id",
	"subject",
	"request_url",
	"skip",
	"requested_scope",
	"authenticated_at",
	"requested_at",
	"csrf",
	"oidc_context",
}

var sqlParamsConsentRequest = append(sqlParamsAuthenticationRequest, "forced_subject_identifier")

var sqlParamsConsentRequestHandled = []string{
	"challenge",
	"granted_scope",
	"remember",
	"remember_for",
	"authenticated_at",
	"error",
	"requested_at",
	"session_access_token",
	"session_id_token",
	"was_used",
}

var sqlParamsAuthSession = []string{
	"id",
	"authenticated_at",
	"subject",
}

type sqlAuthenticationRequest struct {
	OpenIDConnectContext string     `db:"oidc_context"`
	Client               string     `db:"client_id"`
	Subject              string     `db:"subject"`
	RequestURL           string     `db:"request_url"`
	Skip                 bool       `db:"skip"`
	Challenge            string     `db:"challenge"`
	RequestedScope       string     `db:"requested_scope"`
	Verifier             string     `db:"verifier"`
	CSRF                 string     `db:"csrf"`
	AuthenticatedAt      *time.Time `db:"authenticated_at"`
	RequestedAt          time.Time  `db:"requested_at"`
}

type sqlConsentRequest struct {
	sqlAuthenticationRequest
	ForcedSubjectIdentifier string `db:"forced_subject_identifier"`
}

func toMySQLDateHack(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func fromMySQLDateHack(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func newSQLConsentRequest(c *ConsentRequest) (*sqlConsentRequest, error) {
	oidc, err := json.Marshal(c.OpenIDConnectContext)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return &sqlConsentRequest{
		sqlAuthenticationRequest: sqlAuthenticationRequest{
			OpenIDConnectContext: string(oidc),
			Client:               c.Client.GetID(),
			Subject:              c.Subject,
			RequestURL:           c.RequestURL,
			Skip:                 c.Skip,
			Challenge:            c.Challenge,
			RequestedScope:       strings.Join(c.RequestedScope, "|"),
			Verifier:             c.Verifier,
			CSRF:                 c.CSRF,
			AuthenticatedAt:      toMySQLDateHack(c.AuthenticatedAt),
			RequestedAt:          c.RequestedAt,
		},
		ForcedSubjectIdentifier: c.ForceSubjectIdentifier,
	}, nil
}

func newSQLAuthenticationRequest(c *AuthenticationRequest) (*sqlAuthenticationRequest, error) {
	oidc, err := json.Marshal(c.OpenIDConnectContext)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return &sqlAuthenticationRequest{
		OpenIDConnectContext: string(oidc),
		Client:               c.Client.GetID(),
		Subject:              c.Subject,
		RequestURL:           c.RequestURL,
		Skip:                 c.Skip,
		Challenge:            c.Challenge,
		RequestedScope:       strings.Join(c.RequestedScope, "|"),
		Verifier:             c.Verifier,
		CSRF:                 c.CSRF,
		AuthenticatedAt:      toMySQLDateHack(c.AuthenticatedAt),
		RequestedAt:          c.RequestedAt,
	}, nil
}

func (s *sqlAuthenticationRequest) toAuthenticationRequest(client *client.Client) (*AuthenticationRequest, error) {
	var oidc OpenIDConnectContext
	if err := json.Unmarshal([]byte(s.OpenIDConnectContext), &oidc); err != nil {
		return nil, errors.WithStack(err)
	}

	return &AuthenticationRequest{
		OpenIDConnectContext: &oidc,
		Client:               client,
		Subject:              s.Subject,
		RequestURL:           s.RequestURL,
		Skip:                 s.Skip,
		Challenge:            s.Challenge,
		RequestedScope:       stringsx.Splitx(s.RequestedScope, "|"),
		Verifier:             s.Verifier,
		CSRF:                 s.CSRF,
		AuthenticatedAt:      fromMySQLDateHack(s.AuthenticatedAt),
		RequestedAt:          s.RequestedAt,
	}, nil
}

func (s *sqlConsentRequest) toConsentRequest(client *client.Client) (*ConsentRequest, error) {
	var oidc OpenIDConnectContext
	if err := json.Unmarshal([]byte(s.OpenIDConnectContext), &oidc); err != nil {
		return nil, errors.WithStack(err)
	}

	return &ConsentRequest{
		OpenIDConnectContext:   &oidc,
		Client:                 client,
		Subject:                s.Subject,
		RequestURL:             s.RequestURL,
		Skip:                   s.Skip,
		Challenge:              s.Challenge,
		RequestedScope:         stringsx.Splitx(s.RequestedScope, "|"),
		Verifier:               s.Verifier,
		CSRF:                   s.CSRF,
		AuthenticatedAt:        fromMySQLDateHack(s.AuthenticatedAt),
		ForceSubjectIdentifier: s.ForcedSubjectIdentifier,
		RequestedAt:            s.RequestedAt,
	}, nil
}

type sqlHandledConsentRequest struct {
	GrantedScope       string     `db:"granted_scope"`
	SessionIDToken     string     `db:"session_id_token"`
	SessionAccessToken string     `db:"session_access_token"`
	Remember           bool       `db:"remember"`
	RememberFor        int        `db:"remember_for"`
	Error              string     `db:"error"`
	Challenge          string     `db:"challenge"`
	RequestedAt        time.Time  `db:"requested_at"`
	WasUsed            bool       `db:"was_used"`
	AuthenticatedAt    *time.Time `db:"authenticated_at"`
}

func newSQLHandledConsentRequest(c *HandledConsentRequest) (*sqlHandledConsentRequest, error) {
	sidt := "{}"
	sat := "{}"
	e := "{}"

	if c.Session != nil {
		if len(c.Session.IDToken) > 0 {
			if out, err := json.Marshal(c.Session.IDToken); err != nil {
				return nil, errors.WithStack(err)
			} else {
				sidt = string(out)
			}
		}

		if len(c.Session.AccessToken) > 0 {
			if out, err := json.Marshal(c.Session.AccessToken); err != nil {
				return nil, errors.WithStack(err)
			} else {
				sat = string(out)
			}
		}
	}

	if c.Error != nil {
		if out, err := json.Marshal(c.Error); err != nil {
			return nil, errors.WithStack(err)
		} else {
			e = string(out)
		}
	}

	return &sqlHandledConsentRequest{
		GrantedScope:       strings.Join(c.GrantedScope, "|"),
		SessionIDToken:     sidt,
		SessionAccessToken: sat,
		Remember:           c.Remember,
		RememberFor:        c.RememberFor,
		Error:              e,
		Challenge:          c.Challenge,
		RequestedAt:        c.RequestedAt,
		WasUsed:            c.WasUsed,
		AuthenticatedAt:    toMySQLDateHack(c.AuthenticatedAt),
	}, nil
}

func (s *sqlHandledConsentRequest) toHandledConsentRequest(r *ConsentRequest) (*HandledConsentRequest, error) {
	var idt map[string]interface{}
	var at map[string]interface{}
	var e *RequestDeniedError

	if err := json.Unmarshal([]byte(s.SessionIDToken), &idt); err != nil {
		return nil, errors.WithStack(err)
	}
	if err := json.Unmarshal([]byte(s.SessionAccessToken), &at); err != nil {
		return nil, errors.WithStack(err)
	}

	if len(s.Error) > 0 && s.Error != "{}" {
		e = new(RequestDeniedError)
		if err := json.Unmarshal([]byte(s.Error), &e); err != nil {
			return nil, errors.WithStack(err)
		}
	}

	return &HandledConsentRequest{
		GrantedScope: stringsx.Splitx(s.GrantedScope, "|"),
		RememberFor:  s.RememberFor,
		Remember:     s.Remember,
		Challenge:    s.Challenge,
		RequestedAt:  s.RequestedAt,
		WasUsed:      s.WasUsed,
		Session: &ConsentRequestSessionData{
			IDToken:     idt,
			AccessToken: at,
		},
		Error:           e,
		ConsentRequest:  r,
		AuthenticatedAt: fromMySQLDateHack(s.AuthenticatedAt),
	}, nil
}

type sqlHandledAuthenticationRequest struct {
	Remember               bool       `db:"remember"`
	RememberFor            int        `db:"remember_for"`
	ACR                    string     `db:"acr"`
	Subject                string     `db:"subject"`
	Error                  string     `db:"error"`
	Challenge              string     `db:"challenge"`
	RequestedAt            time.Time  `db:"requested_at"`
	WasUsed                bool       `db:"was_used"`
	AuthenticatedAt        *time.Time `db:"authenticated_at"`
	ForceSubjectIdentifier string     `db:"forced_subject_identifier"`
}

func newSQLHandledAuthenticationRequest(c *HandledAuthenticationRequest) (*sqlHandledAuthenticationRequest, error) {
	e := "{}"

	if c.Error != nil {
		if out, err := json.Marshal(c.Error); err != nil {
			return nil, errors.WithStack(err)
		} else {
			e = string(out)
		}
	}

	return &sqlHandledAuthenticationRequest{
		ACR:                    c.ACR,
		Subject:                c.Subject,
		Remember:               c.Remember,
		RememberFor:            c.RememberFor,
		Error:                  e,
		Challenge:              c.Challenge,
		RequestedAt:            c.RequestedAt,
		WasUsed:                c.WasUsed,
		AuthenticatedAt:        toMySQLDateHack(c.AuthenticatedAt),
		ForceSubjectIdentifier: c.ForceSubjectIdentifier,
	}, nil
}

func (s *sqlHandledAuthenticationRequest) toHandledAuthenticationRequest(a *AuthenticationRequest) (*HandledAuthenticationRequest, error) {
	var e *RequestDeniedError

	if len(s.Error) > 0 && s.Error != "{}" {
		e = new(RequestDeniedError)
		if err := json.Unmarshal([]byte(s.Error), &e); err != nil {
			return nil, errors.WithStack(err)
		}
	}

	return &HandledAuthenticationRequest{
		ForceSubjectIdentifier: s.ForceSubjectIdentifier,
		RememberFor:            s.RememberFor,
		Remember:               s.Remember,
		Challenge:              s.Challenge,
		RequestedAt:            s.RequestedAt,
		WasUsed:                s.WasUsed,
		ACR:                    s.ACR,
		Error:                  e,
		AuthenticationRequest: a,
		Subject:               s.Subject,
		AuthenticatedAt:       fromMySQLDateHack(s.AuthenticatedAt),
	}, nil
}
