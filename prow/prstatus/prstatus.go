/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package prstatus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	gogithub "github.com/google/go-github/github"
	"github.com/shurcooL/githubql"
	"github.com/sirupsen/logrus"
	"golang.org/x/oauth2"

	"k8s.io/test-infra/ghclient"
	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/github"
)

const (
	loginSession   = "github_login"
	githubEndpoint = "https://api.github.com"
	tokenSession   = "access-token-session"
	tokenKey       = "access-token"
	loginKey       = "login"
)

type githubClient interface {
	Query(context.Context, interface{}, map[string]interface{}) error
}

type githubRestfulClient interface {
	GetUser(login string) (*gogithub.User, error)
}

type pullRequestRestfulClient struct {
	*ghclient.Client
}

func (grc pullRequestRestfulClient) GetUser(login string) (*gogithub.User, error) {
	return grc.Client.GetUser(login)
}

// PullRequestQueryHandler defines an interface that query handlers should implement.
type PullRequestQueryHandler interface {
	QueryPullRequests(context.Context, githubClient, string) ([]PullRequest, error)
}

// UserData represents data returned to client request to the endpoint. It has a flag that indicates
// whether the user has logged in his github or not and list of open pull requests owned by the
// user.
type UserData struct {
	Login        bool
	PullRequests []PullRequest
}

// Dashboard Agent is responsible for handling request to /pr-status endpoint. It will serve
// list of open pull requests owned by the user.
type DashboardAgent struct {
	repos []string
	goac  *config.GithubOAuthConfig

	log *logrus.Entry
}

type Label struct {
	ID   githubql.ID
	Name githubql.String
}

type PullRequest struct {
	Number githubql.Int
	Merged githubql.Boolean
	Title  githubql.String
	Author struct {
		Login githubql.String
	}
	BaseRef struct {
		Name   githubql.String
		Prefix githubql.String
	}
	HeadRefOID githubql.String `graphql:"headRefOid"`
	Repository struct {
		Name          githubql.String
		NameWithOwner githubql.String
		Owner         struct {
			Login githubql.String
		}
	}
	Labels struct {
		Nodes []struct {
			Label Label `graphql:"... on Label"`
		}
	} `graphql:"labels(first: 100)"`
	Milestone struct {
		ID     githubql.ID
		Closed githubql.Boolean
	}
}

type UserLoginQuery struct {
	Viewer struct {
		Login githubql.String
	}
}

type searchQuery struct {
	RateLimit struct {
		Cost      githubql.Int
		Remaining githubql.Int
	}
	Search struct {
		PageInfo struct {
			HasNextPage githubql.Boolean
			EndCursor   githubql.String
		}
		Nodes []struct {
			PullRequest PullRequest `graphql:"... on PullRequest"`
		}
	} `graphql:"search(type: ISSUE, first: 100, after: $searchCursor, query: $query)"`
}

// Returns new user dashboard agent.
func NewDashboardAgent(repos []string, config *config.GithubOAuthConfig, log *logrus.Entry) *DashboardAgent {
	return &DashboardAgent{
		repos: repos,
		goac:  config,
		log:   log,
	}
}

// HandlePrStatus returns a http handler function that handles request to /pr-status
// endpoint. The handler takes user access token stored in the cookie to query to Github on behalf
// of the user and serve the data in return. The Query handler is passed to the method so as it
// can be mocked in the unit test..
func (da *DashboardAgent) HandlePrStatus(queryHandler PullRequestQueryHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serverError := func(action string, err error) {
			da.log.WithError(err).Errorf("Error %s.", action)
			msg := fmt.Sprintf("500 Internal server error %s: %v", action, err)
			http.Error(w, msg, http.StatusInternalServerError)
		}

		session, err := da.goac.CookieStore.Get(r, tokenSession)
		if err != nil {
			serverError("Error with getting git token session.", err)
			return
		}
		token, ok := session.Values[tokenKey].(*oauth2.Token)
		data := UserData{
			Login: false,
		}

		var user *gogithub.User
		if ok && token.Valid() {
			// If access token exist, get user login using the access token. This is a chance
			// to validate whether the access token is consumable or not. If not, invalidate the
			// session.
			grc := pullRequestRestfulClient{ghclient.NewClient(token.AccessToken, false)}
			var err error
			user, err = grc.GetUser("")
			if err != nil {
				if strings.Contains(err.Error(), "401") {
					// Invalidate access token session
					session.Options.MaxAge = -1
					if err := session.Save(r, w); err != nil {
						serverError("Error with saving invalidated session", err)
						return
					}
					// Invalidate github login session
					http.SetCookie(w, &http.Cookie{
						Name:    loginSession,
						Path:    "/",
						Expires: time.Now().Add(-time.Hour * 24),
						MaxAge:  -1,
						Secure:  true,
					})
				} else {
					serverError("Error with getting user login", err)
					return
				}
			}
		}

		if user != nil {
			login := *user.Login
			data.Login = true
			// Saves login. We save the login under 2 cookies. One for the use of client to render the
			// data and one encoded for server to verify the identity of the authenticated user.
			http.SetCookie(w, &http.Cookie{
				Name:    loginSession,
				Value:   login,
				Path:    "/",
				Expires: time.Now().Add(time.Hour * 24 * 30),
				Secure:  true,
			})
			session.Values[loginKey] = login
			if err := session.Save(r, w); err != nil {
				serverError("Save oauth session", err)
				return
			}
			// Construct query
			ghc := github.NewClient(token.AccessToken, githubEndpoint)
			query := da.ConstructSearchQuery(login)
			if err := r.ParseForm(); err == nil {
				if q := r.Form.Get("query"); q != "" {
					query = q
				}
			}
			// If neither repo nor org is specified in the search query. We limit the search to repos that
			// are configured with either Prow or Tide.
			if !queryConstrainsRepos(query) {
				for _, v := range da.repos {
					query += fmt.Sprintf(" repo:\"%s\"", v)
				}
			}
			pullRequests, err := queryHandler.QueryPullRequests(context.Background(), ghc, query)
			if err != nil {
				serverError("Error with querying user data.", err)
				return
			} else {
				data.PullRequests = pullRequests
			}
		}
		marshaledData, err := json.Marshal(data)
		if err != nil {
			da.log.WithError(err).Error("Error with marshalling user data.")
		}

		if v := r.URL.Query().Get("var"); v != "" {
			fmt.Fprintf(w, "var %s = ", v)
			w.Write(marshaledData)
			io.WriteString(w, ";")
		} else {
			w.Write(marshaledData)
		}
	}
}

// Query function that returns a list of open pull requests owned by the user whose access token
// is consumed by the github client.
func (da *DashboardAgent) QueryPullRequests(ctx context.Context, ghc githubClient, query string) ([]PullRequest, error) {
	var prs []PullRequest
	vars := map[string]interface{}{
		"query":        (githubql.String)(query),
		"searchCursor": (*githubql.String)(nil),
	}
	var totalCost int
	var remaining int
	for {
		sq := searchQuery{}
		if err := ghc.Query(ctx, &sq, vars); err != nil {
			return nil, err
		}
		totalCost += int(sq.RateLimit.Cost)
		remaining = int(sq.RateLimit.Remaining)
		for _, n := range sq.Search.Nodes {
			prs = append(prs, n.PullRequest)
		}
		if !sq.Search.PageInfo.HasNextPage {
			break
		}
		vars["searchCursor"] = githubql.NewString(sq.Search.PageInfo.EndCursor)
	}
	da.log.Infof("Search for query \"%s\" cost %d point(s). %d remaining.", query, totalCost, remaining)
	return prs, nil
}

func (da *DashboardAgent) ConstructSearchQuery(login string) string {
	tokens := []string{"is:pr", "state:open", "author:" + login}
	for i := range da.repos {
		tokens = append(tokens, fmt.Sprintf("repo:\"%s\"", da.repos[i]))
	}
	return strings.Join(tokens, " ")
}

func queryConstrainsRepos(q string) bool {
	tkns := strings.Split(q, " ")
	for _, tkn := range tkns {
		if strings.HasPrefix(tkn, "org:") || strings.HasPrefix(tkn, "repo:") {
			return true
		}
	}
	return false
}
