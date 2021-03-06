// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package api

import (
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"strings"
	"time"

	"github.com/mattermost/mattermost-server/api4"
	"github.com/mattermost/mattermost-server/app"
	"github.com/mattermost/mattermost-server/model"
	"github.com/mattermost/mattermost-server/store"
	"github.com/mattermost/mattermost-server/store/sqlstore"
	"github.com/mattermost/mattermost-server/store/storetest"
	"github.com/mattermost/mattermost-server/utils"
	"github.com/mattermost/mattermost-server/wsapi"

	l4g "github.com/alecthomas/log4go"
)

type TestHelper struct {
	App            *app.App
	tempConfigPath string

	BasicClient  *model.Client
	BasicTeam    *model.Team
	BasicUser    *model.User
	BasicUser2   *model.User
	BasicChannel *model.Channel
	BasicPost    *model.Post
	PinnedPost   *model.Post

	SystemAdminClient  *model.Client
	SystemAdminTeam    *model.Team
	SystemAdminUser    *model.User
	SystemAdminChannel *model.Channel
}

type persistentTestStore struct {
	store.Store
}

func (*persistentTestStore) Close() {}

var testStoreContainer *storetest.RunningContainer
var testStore *persistentTestStore

// UseTestStore sets the container and corresponding settings to use for tests. Once the tests are
// complete (e.g. at the end of your TestMain implementation), you should call StopTestStore.
func UseTestStore(container *storetest.RunningContainer, settings *model.SqlSettings) {
	testStoreContainer = container
	testStore = &persistentTestStore{store.NewLayeredStore(sqlstore.NewSqlSupplier(*settings, nil), nil, nil)}
}

func StopTestStore() {
	if testStoreContainer != nil {
		testStoreContainer.Stop()
		testStoreContainer = nil
	}
}

func setupTestHelper(enterprise bool) *TestHelper {
	permConfig, err := os.Open(utils.FindConfigFile("config.json"))
	if err != nil {
		panic(err)
	}
	defer permConfig.Close()
	tempConfig, err := ioutil.TempFile("", "")
	if err != nil {
		panic(err)
	}
	_, err = io.Copy(tempConfig, permConfig)
	tempConfig.Close()
	if err != nil {
		panic(err)
	}

	options := []app.Option{app.ConfigFile(tempConfig.Name()), app.DisableConfigWatch}
	if testStore != nil {
		options = append(options, app.StoreOverride(testStore))
	}

	a, err := app.New(options...)
	if err != nil {
		panic(err)
	}

	th := &TestHelper{
		App:            a,
		tempConfigPath: tempConfig.Name(),
	}

	th.App.UpdateConfig(func(cfg *model.Config) {
		*cfg.TeamSettings.MaxUsersPerTeam = 50
		*cfg.RateLimitSettings.Enable = false
		cfg.EmailSettings.SendEmailNotifications = true
		*cfg.ServiceSettings.EnableAPIv3 = true
	})
	prevListenAddress := *th.App.Config().ServiceSettings.ListenAddress
	if testStore != nil {
		th.App.UpdateConfig(func(cfg *model.Config) { *cfg.ServiceSettings.ListenAddress = ":0" })
	}
	serverErr := th.App.StartServer()
	if serverErr != nil {
		panic(serverErr)
	}

	th.App.UpdateConfig(func(cfg *model.Config) { *cfg.ServiceSettings.ListenAddress = prevListenAddress })
	api4.Init(th.App, th.App.Srv.Router, false)
	Init(th.App, th.App.Srv.Router)
	wsapi.Init(th.App, th.App.Srv.WebSocketRouter)
	th.App.Srv.Store.MarkSystemRanUnitTests()
	th.App.DoAdvancedPermissionsMigration()

	th.App.UpdateConfig(func(cfg *model.Config) { *cfg.TeamSettings.EnableOpenServer = true })

	if enterprise {
		th.App.SetLicense(model.NewTestLicense())
	} else {
		th.App.SetLicense(nil)
	}

	return th
}

func SetupEnterprise() *TestHelper {
	return setupTestHelper(true)
}

func Setup() *TestHelper {
	return setupTestHelper(false)
}

func (me *TestHelper) InitBasic() *TestHelper {
	me.waitForConnectivity()

	me.BasicClient = me.CreateClient()
	me.BasicUser = me.CreateUser(me.BasicClient)
	me.App.UpdateUserRoles(me.BasicUser.Id, model.SYSTEM_USER_ROLE_ID, false)
	me.LoginBasic()
	me.BasicTeam = me.CreateTeam(me.BasicClient)
	me.LinkUserToTeam(me.BasicUser, me.BasicTeam)
	me.UpdateUserToNonTeamAdmin(me.BasicUser, me.BasicTeam)
	me.BasicUser2 = me.CreateUser(me.BasicClient)
	me.LinkUserToTeam(me.BasicUser2, me.BasicTeam)
	me.BasicClient.SetTeamId(me.BasicTeam.Id)
	me.BasicChannel = me.CreateChannel(me.BasicClient, me.BasicTeam)
	me.BasicPost = me.CreatePost(me.BasicClient, me.BasicChannel)

	pinnedPostChannel := me.CreateChannel(me.BasicClient, me.BasicTeam)
	me.PinnedPost = me.CreatePinnedPost(me.BasicClient, pinnedPostChannel)

	return me
}

func (me *TestHelper) InitSystemAdmin() *TestHelper {
	me.waitForConnectivity()

	me.SystemAdminClient = me.CreateClient()
	me.SystemAdminUser = me.CreateUser(me.SystemAdminClient)
	me.SystemAdminUser.Password = "Password1"
	me.LoginSystemAdmin()
	me.SystemAdminTeam = me.CreateTeam(me.SystemAdminClient)
	me.LinkUserToTeam(me.SystemAdminUser, me.SystemAdminTeam)
	me.SystemAdminClient.SetTeamId(me.SystemAdminTeam.Id)
	me.App.UpdateUserRoles(me.SystemAdminUser.Id, model.SYSTEM_USER_ROLE_ID+" "+model.SYSTEM_ADMIN_ROLE_ID, false)
	me.SystemAdminChannel = me.CreateChannel(me.SystemAdminClient, me.SystemAdminTeam)

	return me
}

func (me *TestHelper) waitForConnectivity() {
	for i := 0; i < 1000; i++ {
		conn, err := net.Dial("tcp", fmt.Sprintf("localhost:%v", me.App.Srv.ListenAddr.Port))
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(time.Millisecond * 20)
	}
	panic("unable to connect")
}

func (me *TestHelper) CreateClient() *model.Client {
	return model.NewClient(fmt.Sprintf("http://localhost:%v", me.App.Srv.ListenAddr.Port))
}

func (me *TestHelper) CreateWebSocketClient() (*model.WebSocketClient, *model.AppError) {
	return model.NewWebSocketClient(fmt.Sprintf("ws://localhost:%v", me.App.Srv.ListenAddr.Port), me.BasicClient.AuthToken)
}

func (me *TestHelper) CreateTeam(client *model.Client) *model.Team {
	id := model.NewId()
	team := &model.Team{
		DisplayName: "dn_" + id,
		Name:        GenerateTestTeamName(),
		Email:       me.GenerateTestEmail(),
		Type:        model.TEAM_OPEN,
	}

	utils.DisableDebugLogForTest()
	r := client.Must(client.CreateTeam(team)).Data.(*model.Team)
	utils.EnableDebugLogForTest()
	return r
}

func (me *TestHelper) CreateUser(client *model.Client) *model.User {
	id := model.NewId()

	user := &model.User{
		Email:    me.GenerateTestEmail(),
		Username: "un_" + id,
		Nickname: "nn_" + id,
		Password: "Password1",
	}

	utils.DisableDebugLogForTest()
	ruser := client.Must(client.CreateUser(user, "")).Data.(*model.User)
	ruser.Password = "Password1"
	store.Must(me.App.Srv.Store.User().VerifyEmail(ruser.Id))
	utils.EnableDebugLogForTest()
	return ruser
}

func (me *TestHelper) LinkUserToTeam(user *model.User, team *model.Team) {
	utils.DisableDebugLogForTest()

	err := me.App.JoinUserToTeam(team, user, "")
	if err != nil {
		l4g.Error(err.Error())
		l4g.Close()
		time.Sleep(time.Second)
		panic(err)
	}

	utils.EnableDebugLogForTest()
}

func (me *TestHelper) UpdateUserToTeamAdmin(user *model.User, team *model.Team) {
	utils.DisableDebugLogForTest()

	tm := &model.TeamMember{TeamId: team.Id, UserId: user.Id, Roles: model.TEAM_USER_ROLE_ID + " " + model.TEAM_ADMIN_ROLE_ID}
	if tmr := <-me.App.Srv.Store.Team().UpdateMember(tm); tmr.Err != nil {
		utils.EnableDebugLogForTest()
		l4g.Error(tmr.Err.Error())
		l4g.Close()
		time.Sleep(time.Second)
		panic(tmr.Err)
	}
	utils.EnableDebugLogForTest()
}

func (me *TestHelper) UpdateUserToNonTeamAdmin(user *model.User, team *model.Team) {
	utils.DisableDebugLogForTest()

	tm := &model.TeamMember{TeamId: team.Id, UserId: user.Id, Roles: model.TEAM_USER_ROLE_ID}
	if tmr := <-me.App.Srv.Store.Team().UpdateMember(tm); tmr.Err != nil {
		utils.EnableDebugLogForTest()
		l4g.Error(tmr.Err.Error())
		l4g.Close()
		time.Sleep(time.Second)
		panic(tmr.Err)
	}
	utils.EnableDebugLogForTest()
}

func (me *TestHelper) MakeUserChannelAdmin(user *model.User, channel *model.Channel) {
	utils.DisableDebugLogForTest()

	if cmr := <-me.App.Srv.Store.Channel().GetMember(channel.Id, user.Id); cmr.Err == nil {
		cm := cmr.Data.(*model.ChannelMember)
		cm.Roles = "channel_admin channel_user"
		if sr := <-me.App.Srv.Store.Channel().UpdateMember(cm); sr.Err != nil {
			utils.EnableDebugLogForTest()
			panic(sr.Err)
		}
	} else {
		utils.EnableDebugLogForTest()
		panic(cmr.Err)
	}

	utils.EnableDebugLogForTest()
}

func (me *TestHelper) MakeUserChannelUser(user *model.User, channel *model.Channel) {
	utils.DisableDebugLogForTest()

	if cmr := <-me.App.Srv.Store.Channel().GetMember(channel.Id, user.Id); cmr.Err == nil {
		cm := cmr.Data.(*model.ChannelMember)
		cm.Roles = "channel_user"
		if sr := <-me.App.Srv.Store.Channel().UpdateMember(cm); sr.Err != nil {
			utils.EnableDebugLogForTest()
			panic(sr.Err)
		}
	} else {
		utils.EnableDebugLogForTest()
		panic(cmr.Err)
	}

	utils.EnableDebugLogForTest()
}

func (me *TestHelper) CreateChannel(client *model.Client, team *model.Team) *model.Channel {
	return me.createChannel(client, team, model.CHANNEL_OPEN)
}

func (me *TestHelper) CreatePrivateChannel(client *model.Client, team *model.Team) *model.Channel {
	return me.createChannel(client, team, model.CHANNEL_PRIVATE)
}

func (me *TestHelper) createChannel(client *model.Client, team *model.Team, channelType string) *model.Channel {
	id := model.NewId()

	channel := &model.Channel{
		DisplayName: "dn_" + id,
		Name:        "name_" + id,
		Type:        channelType,
		TeamId:      team.Id,
	}

	utils.DisableDebugLogForTest()
	r := client.Must(client.CreateChannel(channel)).Data.(*model.Channel)
	utils.EnableDebugLogForTest()
	return r
}

func (me *TestHelper) CreatePost(client *model.Client, channel *model.Channel) *model.Post {
	id := model.NewId()

	post := &model.Post{
		ChannelId: channel.Id,
		Message:   "message_" + id,
	}

	utils.DisableDebugLogForTest()
	r := client.Must(client.CreatePost(post)).Data.(*model.Post)
	utils.EnableDebugLogForTest()
	return r
}

func (me *TestHelper) CreatePinnedPost(client *model.Client, channel *model.Channel) *model.Post {
	id := model.NewId()

	post := &model.Post{
		ChannelId: channel.Id,
		Message:   "message_" + id,
		IsPinned:  true,
	}

	utils.DisableDebugLogForTest()
	r := client.Must(client.CreatePost(post)).Data.(*model.Post)
	utils.EnableDebugLogForTest()
	return r
}

func (me *TestHelper) LoginBasic() {
	utils.DisableDebugLogForTest()
	me.BasicClient.Must(me.BasicClient.Login(me.BasicUser.Email, me.BasicUser.Password))
	utils.EnableDebugLogForTest()
}

func (me *TestHelper) LoginBasic2() {
	utils.DisableDebugLogForTest()
	me.BasicClient.Must(me.BasicClient.Login(me.BasicUser2.Email, me.BasicUser2.Password))
	utils.EnableDebugLogForTest()
}

func (me *TestHelper) LoginSystemAdmin() {
	utils.DisableDebugLogForTest()
	me.SystemAdminClient.Must(me.SystemAdminClient.Login(me.SystemAdminUser.Email, me.SystemAdminUser.Password))
	utils.EnableDebugLogForTest()
}

func (me *TestHelper) GenerateTestEmail() string {
	if me.App.Config().EmailSettings.SMTPServer != "dockerhost" && os.Getenv("CI_INBUCKET_PORT") == "" {
		return strings.ToLower("success+" + model.NewId() + "@simulator.amazonses.com")
	}
	return strings.ToLower(model.NewId() + "@dockerhost")
}

func GenerateTestTeamName() string {
	return "faketeam" + model.NewRandomString(6)
}

func (me *TestHelper) TearDown() {
	me.App.Shutdown()
	os.Remove(me.tempConfigPath)
	if err := recover(); err != nil {
		StopTestStore()
		panic(err)
	}
}

func (me *TestHelper) SaveDefaultRolePermissions() map[string][]string {
	utils.DisableDebugLogForTest()

	results := make(map[string][]string)

	for _, roleName := range []string{
		"system_user",
		"system_admin",
		"team_user",
		"team_admin",
		"channel_user",
		"channel_admin",
	} {
		role, err1 := me.App.GetRoleByName(roleName)
		if err1 != nil {
			utils.EnableDebugLogForTest()
			panic(err1)
		}

		results[roleName] = role.Permissions
	}

	utils.EnableDebugLogForTest()
	return results
}

func (me *TestHelper) RestoreDefaultRolePermissions(data map[string][]string) {
	utils.DisableDebugLogForTest()

	for roleName, permissions := range data {
		role, err1 := me.App.GetRoleByName(roleName)
		if err1 != nil {
			utils.EnableDebugLogForTest()
			panic(err1)
		}

		if strings.Join(role.Permissions, " ") == strings.Join(permissions, " ") {
			continue
		}

		role.Permissions = permissions

		_, err2 := me.App.UpdateRole(role)
		if err2 != nil {
			utils.EnableDebugLogForTest()
			panic(err2)
		}
	}

	utils.EnableDebugLogForTest()
}

func (me *TestHelper) RemovePermissionFromRole(permission string, roleName string) {
	utils.DisableDebugLogForTest()

	role, err1 := me.App.GetRoleByName(roleName)
	if err1 != nil {
		utils.EnableDebugLogForTest()
		panic(err1)
	}

	var newPermissions []string
	for _, p := range role.Permissions {
		if p != permission {
			newPermissions = append(newPermissions, p)
		}
	}

	if strings.Join(role.Permissions, " ") == strings.Join(newPermissions, " ") {
		utils.EnableDebugLogForTest()
		return
	}

	role.Permissions = newPermissions

	_, err2 := me.App.UpdateRole(role)
	if err2 != nil {
		utils.EnableDebugLogForTest()
		panic(err2)
	}

	utils.EnableDebugLogForTest()
}

func (me *TestHelper) AddPermissionToRole(permission string, roleName string) {
	utils.DisableDebugLogForTest()

	role, err1 := me.App.GetRoleByName(roleName)
	if err1 != nil {
		utils.EnableDebugLogForTest()
		panic(err1)
	}

	for _, existingPermission := range role.Permissions {
		if existingPermission == permission {
			utils.EnableDebugLogForTest()
			return
		}
	}

	role.Permissions = append(role.Permissions, permission)

	_, err2 := me.App.UpdateRole(role)
	if err2 != nil {
		utils.EnableDebugLogForTest()
		panic(err2)
	}

	utils.EnableDebugLogForTest()
}
