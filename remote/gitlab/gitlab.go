package gitlab

import (
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/drone/drone/model"
	"github.com/drone/drone/shared/envconfig"
	"github.com/drone/drone/shared/httputil"
	"github.com/drone/drone/shared/oauth2"
	"github.com/drone/drone/shared/token"

	"github.com/drone/drone/remote/gitlab/client"
)

const (
	DefaultScope = "api"
)

type Gitlab struct {
	URL          string
	Client       string
	Secret       string
	AllowedOrgs  []string
	CloneMode    string
	Open         bool
	PrivateMode  bool
	SkipVerify   bool
	HideArchives bool
	Search       bool
}

func Load(env envconfig.Env) *Gitlab {
	config := env.String("REMOTE_CONFIG", "")

	url_, err := url.Parse(config)
	if err != nil {
		panic(err)
	}
	params := url_.Query()
	url_.RawQuery = ""

	gitlab := Gitlab{}
	gitlab.URL = url_.String()
	gitlab.Client = params.Get("client_id")
	gitlab.Secret = params.Get("client_secret")
	gitlab.AllowedOrgs = params["orgs"]
	gitlab.SkipVerify, _ = strconv.ParseBool(params.Get("skip_verify"))
	gitlab.HideArchives, _ = strconv.ParseBool(params.Get("hide_archives"))
	gitlab.Open, _ = strconv.ParseBool(params.Get("open"))

	switch params.Get("clone_mode") {
	case "oauth":
		gitlab.CloneMode = "oauth"
	default:
		gitlab.CloneMode = "token"
	}

	// this is a temp workaround
	gitlab.Search, _ = strconv.ParseBool(params.Get("search"))

	return &gitlab
}

// Login authenticates the session and returns the
// remote user details.
func (g *Gitlab) Login(res http.ResponseWriter, req *http.Request) (*model.User, bool, error) {

	var config = &oauth2.Config{
		ClientId:     g.Client,
		ClientSecret: g.Secret,
		Scope:        DefaultScope,
		AuthURL:      fmt.Sprintf("%s/oauth/authorize", g.URL),
		TokenURL:     fmt.Sprintf("%s/oauth/token", g.URL),
		RedirectURL:  fmt.Sprintf("%s/authorize", httputil.GetURL(req)),
	}

	trans_ := &http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: g.SkipVerify},
	}

	// get the OAuth code
	var code = req.FormValue("code")
	if len(code) == 0 {
		http.Redirect(res, req, config.AuthCodeURL("drone"), http.StatusSeeOther)
		return nil, false, nil
	}

	var trans = &oauth2.Transport{Config: config, Transport: trans_}
	var token_, err = trans.Exchange(code)
	if err != nil {
		return nil, false, fmt.Errorf("Error exchanging token. %s", err)
	}

	client := NewClient(g.URL, token_.AccessToken, g.SkipVerify)
	login, err := client.CurrentUser()
	if err != nil {
		return nil, false, err
	}

	if len(g.AllowedOrgs) != 0 {
		groups, err := client.AllGroups()
		if err != nil {
			return nil, false, fmt.Errorf("Could not check org membership. %s", err)
		}

		var member bool
		for _, group := range groups {
			for _, allowedOrg := range g.AllowedOrgs {
				if group.Path == allowedOrg {
					member = true
					break
				}
			}
		}

		if !member {
			return nil, false, fmt.Errorf("User does not belong to correct group. Must belong to %v", g.AllowedOrgs)
		}
	}

	user := &model.User{}
	user.Login = login.Username
	user.Email = login.Email
	user.Token = token_.AccessToken
	user.Secret = token_.RefreshToken

	if strings.HasPrefix(login.AvatarUrl, "http") {
		user.Avatar = login.AvatarUrl
	} else {
		user.Avatar = g.URL + "/" + login.AvatarUrl
	}

	return user, g.Open, nil
}

func (g *Gitlab) Auth(token, secret string) (string, error) {
	client := NewClient(g.URL, token, g.SkipVerify)
	login, err := client.CurrentUser()
	if err != nil {
		return "", err
	}
	return login.Username, nil
}

// Repo fetches the named repository from the remote system.
func (g *Gitlab) Repo(u *model.User, owner, name string) (*model.Repo, error) {
	client := NewClient(g.URL, u.Token, g.SkipVerify)
	id, err := GetProjectId(g, client, owner, name)
	if err != nil {
		return nil, err
	}
	repo_, err := client.Project(id)
	if err != nil {
		return nil, err
	}

	repo := &model.Repo{}
	repo.Owner = owner
	repo.Name = name
	repo.FullName = repo_.PathWithNamespace
	repo.Link = repo_.Url
	repo.Clone = repo_.HttpRepoUrl
	repo.Branch = "master"

	repo.Avatar = repo_.AvatarUrl

	if len(repo.Avatar) != 0 && !strings.HasPrefix(repo.Avatar, "http") {
		repo.Avatar = fmt.Sprintf("%s/%s", g.URL, repo.Avatar)
	}

	if repo_.DefaultBranch != "" {
		repo.Branch = repo_.DefaultBranch
	}

	if g.PrivateMode {
		repo.IsPrivate = true
	} else {
		repo.IsPrivate = !repo_.Public
	}

	return repo, err
}

// Repos fetches a list of repos from the remote system.
func (g *Gitlab) Repos(u *model.User) ([]*model.RepoLite, error) {
	client := NewClient(g.URL, u.Token, g.SkipVerify)

	var repos = []*model.RepoLite{}

	all, err := client.AllProjects(g.HideArchives)
	if err != nil {
		return repos, err
	}

	for _, repo := range all {
		var parts = strings.Split(repo.PathWithNamespace, "/")
		var owner = parts[0]
		var name = parts[1]
		var avatar = repo.AvatarUrl

		if len(avatar) != 0 && !strings.HasPrefix(avatar, "http") {
			avatar = fmt.Sprintf("%s/%s", g.URL, avatar)
		}

		repos = append(repos, &model.RepoLite{
			Owner:    owner,
			Name:     name,
			FullName: repo.PathWithNamespace,
			Avatar:   avatar,
		})
	}

	return repos, err
}

// Perm fetches the named repository from the remote system.
func (g *Gitlab) Perm(u *model.User, owner, name string) (*model.Perm, error) {

	client := NewClient(g.URL, u.Token, g.SkipVerify)
	id, err := GetProjectId(g, client, owner, name)
	if err != nil {
		return nil, err
	}

	repo, err := client.Project(id)
	if err != nil {
		return nil, err
	}

	// repo owner is granted full access
	if repo.Owner != nil && repo.Owner.Username == u.Login {
		return &model.Perm{true, true, true}, nil
	}

	// check permission for current user
	m := &model.Perm{}
	m.Admin = IsAdmin(repo)
	m.Pull = IsRead(repo)
	m.Push = IsWrite(repo)
	return m, nil
}

// File fetches a file from the remote repository and returns in string format.
func (g *Gitlab) File(user *model.User, repo *model.Repo, build *model.Build, f string) ([]byte, error) {
	var client = NewClient(g.URL, user.Token, g.SkipVerify)
	id, err := GetProjectId(g, client, repo.Owner, repo.Name)
	if err != nil {
		return nil, err
	}

	out, err := client.RepoRawFile(id, build.Commit, f)
	if err != nil {
		return nil, err
	}
	return out, err
}

// NOTE Currently gitlab doesn't support status for commits and events,
//      also if we want get MR status in gitlab we need implement a special plugin for gitlab,
//      gitlab uses API to fetch build status on client side. But for now we skip this.
func (g *Gitlab) Status(u *model.User, repo *model.Repo, b *model.Build, link string) error {
	client := NewClient(g.URL, u.Token, g.SkipVerify)

	status := getStatus(b.Status)
	desc := getDesc(b.Status)

	client.SetStatus(
		ns(repo.Owner, repo.Name),
		b.Commit,
		status,
		desc,
		strings.Replace(b.Ref, "refs/heads/", "", -1),
		link,
	)

	// Gitlab statuses it's a new feature, just ignore error
	// if gitlab version not support this
	return nil
}

// Netrc returns a .netrc file that can be used to clone
// private repositories from a remote system.
func (g *Gitlab) Netrc(u *model.User, r *model.Repo) (*model.Netrc, error) {
	url_, err := url.Parse(g.URL)
	if err != nil {
		return nil, err
	}
	netrc := &model.Netrc{}
	netrc.Machine = url_.Host

	switch g.CloneMode {
	case "oauth":
		netrc.Login = "oauth2"
		netrc.Password = u.Token
	case "token":
		t := token.New(token.HookToken, r.FullName)
		netrc.Login = "drone-ci-token"
		netrc.Password, err = t.Sign(r.Hash)
	}
	return netrc, err
}

// Activate activates a repository by adding a Post-commit hook and
// a Public Deploy key, if applicable.
func (g *Gitlab) Activate(user *model.User, repo *model.Repo, k *model.Key, link string) error {
	var client = NewClient(g.URL, user.Token, g.SkipVerify)
	id, err := GetProjectId(g, client, repo.Owner, repo.Name)
	if err != nil {
		return err
	}

	uri, err := url.Parse(link)
	if err != nil {
		return err
	}

	droneUrl := fmt.Sprintf("%s://%s", uri.Scheme, uri.Host)
	droneToken := uri.Query().Get("access_token")
	ssl_verify := strconv.FormatBool(!g.SkipVerify)

	return client.AddDroneService(id, map[string]string{
		"token":                   droneToken,
		"drone_url":               droneUrl,
		"enable_ssl_verification": ssl_verify,
	})
}

// Deactivate removes a repository by removing all the post-commit hooks
// which are equal to link and removing the SSH deploy key.
func (g *Gitlab) Deactivate(user *model.User, repo *model.Repo, link string) error {
	var client = NewClient(g.URL, user.Token, g.SkipVerify)
	id, err := GetProjectId(g, client, repo.Owner, repo.Name)
	if err != nil {
		return err
	}

	return client.DeleteDroneService(id)
}

// ParseHook parses the post-commit hook from the Request body
// and returns the required data in a standard format.
func (g *Gitlab) Hook(req *http.Request) (*model.Repo, *model.Build, error) {
	defer req.Body.Close()
	var payload, _ = ioutil.ReadAll(req.Body)
	var parsed, err = client.ParseHook(payload)
	if err != nil {
		return nil, nil, err
	}

	switch parsed.ObjectKind {
	case "merge_request":
		return mergeRequest(parsed, req)
	case "tag_push", "push":
		return push(parsed, req)
	default:
		return nil, nil, nil
	}
}

func mergeRequest(parsed *client.HookPayload, req *http.Request) (*model.Repo, *model.Build, error) {

	repo := &model.Repo{}

	obj := parsed.ObjectAttributes
	if obj == nil {
		return nil, nil, fmt.Errorf("object_attributes key expected in merge request hook")
	}

	target := obj.Target
	source := obj.Source

	if target == nil && source == nil {
		return nil, nil, fmt.Errorf("target and source keys expected in merge request hook")
	} else if target == nil {
		return nil, nil, fmt.Errorf("target key expected in merge request hook")
	} else if source == nil {
		return nil, nil, fmt.Errorf("source key exptected in merge request hook")
	}

	if target.PathWithNamespace != "" {
		var err error
		if repo.Owner, repo.Name, err = ExtractFromPath(target.PathWithNamespace); err != nil {
			return nil, nil, err
		}
		repo.FullName = target.PathWithNamespace
	} else {
		repo.Owner = req.FormValue("owner")
		repo.Name = req.FormValue("name")
		repo.FullName = fmt.Sprintf("%s/%s", repo.Owner, repo.Name)
	}

	repo.Link = target.WebUrl

	if target.GitHttpUrl != "" {
		repo.Clone = target.GitHttpUrl
	} else {
		repo.Clone = target.HttpUrl
	}

	if target.DefaultBranch != "" {
		repo.Branch = target.DefaultBranch
	} else {
		repo.Branch = "master"
	}

	if target.AvatarUrl != "" {
		repo.Avatar = target.AvatarUrl
	}

	build := &model.Build{}
	build.Event = "pull_request"

	lastCommit := obj.LastCommit
	if lastCommit == nil {
		return nil, nil, fmt.Errorf("last_commit key expected in merge request hook")
	}

	build.Message = lastCommit.Message
	build.Commit = lastCommit.Id
	//build.Remote = parsed.ObjectAttributes.Source.HttpUrl

	if obj.SourceProjectId == obj.TargetProjectId {
		build.Ref = fmt.Sprintf("refs/heads/%s", obj.SourceBranch)
	} else {
		build.Ref = fmt.Sprintf("refs/merge-requests/%d/head", obj.IId)
	}

	build.Branch = obj.SourceBranch

	author := lastCommit.Author
	if author == nil {
		return nil, nil, fmt.Errorf("author key expected in merge request hook")
	}

	build.Author = author.Name
	build.Email = author.Email

	if len(build.Email) != 0 {
		build.Avatar = GetUserAvatar(build.Email)
	}

	build.Title = obj.Title
	build.Link = obj.Url

	return repo, build, nil
}

func push(parsed *client.HookPayload, req *http.Request) (*model.Repo, *model.Build, error) {
	repo := &model.Repo{}

	// Since gitlab 8.5, used project instead repository key
	// see https://gitlab.com/gitlab-org/gitlab-ce/blob/master/doc/web_hooks/web_hooks.md#web-hooks
	if project := parsed.Project; project != nil {
		var err error
		if repo.Owner, repo.Name, err = ExtractFromPath(project.PathWithNamespace); err != nil {
			return nil, nil, err
		}

		repo.Avatar = project.AvatarUrl
		repo.Link = project.WebUrl
		repo.Clone = project.GitHttpUrl
		repo.FullName = project.PathWithNamespace
		repo.Branch = project.DefaultBranch

		switch project.VisibilityLevel {
		case 0:
			repo.IsPrivate = true
		case 10:
			repo.IsPrivate = true
		case 20:
			repo.IsPrivate = false
		}
	} else if repository := parsed.Repository; repository != nil {
		repo.Owner = req.FormValue("owner")
		repo.Name = req.FormValue("name")
		repo.Link = repository.URL
		repo.Clone = repository.GitHttpUrl
		repo.Branch = "master"
		repo.FullName = fmt.Sprintf("%s/%s", req.FormValue("owner"), req.FormValue("name"))

		switch repository.VisibilityLevel {
		case 0:
			repo.IsPrivate = true
		case 10:
			repo.IsPrivate = true
		case 20:
			repo.IsPrivate = false
		}
	} else {
		return nil, nil, fmt.Errorf("No project/repository keys given")
	}

	build := &model.Build{}
	build.Event = model.EventPush
	build.Commit = parsed.After
	build.Branch = parsed.Branch()
	build.Ref = parsed.Ref
	// hook.Commit.Remote = cloneUrl

	var head = parsed.Head()
	build.Message = head.Message
	// build.Timestamp = head.Timestamp

	// extracts the commit author (ideally email)
	// from the post-commit hook
	switch {
	case head.Author != nil:
		build.Email = head.Author.Email
		build.Author = parsed.UserName
		if len(build.Email) != 0 {
			build.Avatar = GetUserAvatar(build.Email)
		}
	case head.Author == nil:
		build.Author = parsed.UserName
	}

	if strings.HasPrefix(build.Ref, "refs/tags/") {
		build.Event = model.EventTag
	}

	return repo, build, nil
}

// ¯\_(ツ)_/¯
func (g *Gitlab) Oauth2Transport(r *http.Request) *oauth2.Transport {
	return &oauth2.Transport{
		Config: &oauth2.Config{
			ClientId:     g.Client,
			ClientSecret: g.Secret,
			Scope:        DefaultScope,
			AuthURL:      fmt.Sprintf("%s/oauth/authorize", g.URL),
			TokenURL:     fmt.Sprintf("%s/oauth/token", g.URL),
			RedirectURL:  fmt.Sprintf("%s/authorize", httputil.GetURL(r)),
			//settings.Server.Scheme, settings.Server.Hostname),
		},
		Transport: &http.Transport{
			Proxy:           http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{InsecureSkipVerify: g.SkipVerify},
		},
	}
}

const (
	StatusPending  = "pending"
	StatusRunning  = "running"
	StatusSuccess  = "success"
	StatusFailure  = "failed"
	StatusCanceled = "canceled"
)

const (
	DescPending  = "this build is pending"
	DescRunning  = "this buils is running"
	DescSuccess  = "the build was successful"
	DescFailure  = "the build failed"
	DescCanceled = "the build canceled"
)

// getStatus is a helper functin that converts a Drone
// status to a GitHub status.
func getStatus(status string) string {
	switch status {
	case model.StatusPending:
		return StatusPending
	case model.StatusRunning:
		return StatusRunning
	case model.StatusSuccess:
		return StatusSuccess
	case model.StatusFailure, model.StatusError:
		return StatusFailure
	case model.StatusKilled:
		return StatusCanceled
	default:
		return StatusFailure
	}
}

// getDesc is a helper function that generates a description
// message for the build based on the status.
func getDesc(status string) string {
	switch status {
	case model.StatusPending:
		return DescPending
	case model.StatusRunning:
		return DescRunning
	case model.StatusSuccess:
		return DescSuccess
	case model.StatusFailure, model.StatusError:
		return DescFailure
	case model.StatusKilled:
		return DescCanceled
	default:
		return DescFailure
	}
}
