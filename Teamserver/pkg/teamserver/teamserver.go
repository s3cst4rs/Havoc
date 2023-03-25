package teamserver

import "C"
import (
	"Havoc/pkg/agent"
	"Havoc/pkg/db"
	"Havoc/pkg/webhook"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"Havoc/pkg/service"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/sha3"

	"Havoc/pkg/colors"
	"Havoc/pkg/events"
	"Havoc/pkg/handlers"
	"Havoc/pkg/logger"
	"Havoc/pkg/packager"
	"Havoc/pkg/profile"
	"Havoc/pkg/utils"
)

// maybe move this to cmd

func NewTeamserver() *Teamserver {
	return new(Teamserver)
}

func (t *Teamserver) SetServerFlags(flags TeamserverFlags) {
	t.Flags = flags
}

func (t *Teamserver) Start() {
	logger.Debug("Starting teamserver...")
	var (
		ServerFinished      chan bool
		TeamserverWs        string
		TeamserverPath, err = os.Getwd()
		ListenerCount       int
	)

	if err != nil {
		logger.Error("Couldn't get the current directory: " + err.Error())
		return
	}

	// 命令行中没有指定的话，就从Profile结构体当中取
	if t.Flags.Server.Host == "" {
		t.Flags.Server.Host = t.Profile.ServerHost()
	}

	if t.Flags.Server.Port == "" {
		t.Flags.Server.Port = strconv.Itoa(t.Profile.ServerPort())
	}

	gin.SetMode(gin.ReleaseMode)
	t.Server.Engine = gin.New()

	t.Server.Engine.GET("/", func(context *gin.Context) {
		context.Redirect(http.StatusMovedPermanently, "home/")
	})

	// Catch me if you can
	// 提供给Client做通信处理，这里是WebSocket协议
	t.Server.Engine.GET("/havoc/", func(context *gin.Context) {
		upgrade := websocket.Upgrader{}
		// http协议转WebSocket协议
		WebSocket, err := upgrade.Upgrade(context.Writer, context.Request, nil)
		if err != nil {
			logger.Error("Failed upgrading request")
			return
		}

		var ClientID = utils.GenerateID(6)
		if err != nil {
			fmt.Println("Error accepting: ", err.Error())
			os.Exit(1)
		}

		t.Clients[ClientID] = &Client{
			Username:      "",
			GlobalIP:      WebSocket.RemoteAddr().String(),
			Connection:    WebSocket,
			ClientVersion: "",
			Packager:      packager.NewPackager(),
			Authenticated: false,
		}

		// Handle connections in a new goroutine.
		go t.handleRequest(ClientID)
	})

	// TODO: pass this as a profile/command line flag
	t.Server.Engine.Static("/home", "./bin/static")

	// ExternalC2的处理
	t.Server.Engine.POST("/:endpoint", func(context *gin.Context) {
		var endpoint = context.Request.RequestURI[1:] // 获取根目录后面的内容，http://example.com/foo?bar=baz#qux，这里就是foo?bar=baz#qux

		// 如果有这个endpoint，就执行对应的函数，这里的函数是在handlers包中定义的
		if len(t.Endpoints) > 0 {
			for i := range t.Endpoints {
				if t.Endpoints[i].Endpoint == endpoint {
					t.Endpoints[i].Function(context)
				}
			}
		}
	})

	// 启动Teamserver
	go func(Server string) {
		// Server是host:port
		err := t.Server.Engine.Run(Server)
		if err != nil {
			logger.Error("Failed to start websocket: " + err.Error())
		}
		ServerFinished <- true
		os.Exit(0)
	}(t.Flags.Server.Host + ":" + t.Flags.Server.Port)

	t.WebHooks = webhook.NewWebHook()
	t.Service = service.NewService(t.Server.Engine) // 与扩展的Service与主Engine共用
	t.Service.Events = t                            // 目前只有Teamserver实现了events.EventInterface接口，猜测可能是为了后续扩展
	t.Service.TeamAgents = &t.Agents                // 目前只有TeamAgentsInterface实现了，Agent猜测是用于任务相关的
	t.Clients = make(map[string]*Client)
	t.Listeners = []*Listener{}
	TeamserverWs = "ws://" + t.Flags.Server.Host + ":" + t.Flags.Server.Port
	t.Service.Data.ServerAgents = &t.Agents

	logger.Info("Starting Teamserver on " + colors.BlueUnderline(TeamserverWs))

	/* if we specified a webhook then lets use it. */
	if t.Profile.Config.WebHook != nil {
		// 目前只实现了Discord的WebHook
		if t.Profile.Config.WebHook.Discord != nil {

			var (
				AvatarUrl string
				UserName  string
			)

			if len(t.Profile.Config.WebHook.Discord.AvatarUrl) > 0 {
				AvatarUrl = t.Profile.Config.WebHook.Discord.AvatarUrl
			}

			if len(t.Profile.Config.WebHook.Discord.UserName) > 0 {
				UserName = t.Profile.Config.WebHook.Discord.UserName
			}

			// 设置到Teamserver的WebHook中
			if len(t.Profile.Config.WebHook.Discord.WebHook) > 0 {
				t.WebHooks.SetDiscord(AvatarUrl, UserName, t.Profile.Config.WebHook.Discord.WebHook)
			}

		}

	}

	// start teamserver service
	// 启动ExternalC2
	if t.Profile.Config.Service != nil {
		t.Service.Config = *t.Profile.Config.Service

		if len(t.Service.Config.Endpoint) > 0 {
			t.Service.Start()
			logger.Info("Starting Teamserver service handler on " + colors.BlueUnderline(TeamserverWs+"/"+t.Service.Config.Endpoint))
		} else {
			logger.Error("Teamserver service error: Endpoint not specified")
		}
	}

	/* now load up our db or start a new one if none exist */
	if t.DB, err = db.DatabaseNew(TeamserverPath + "/data/havoc.db"); err != nil {
		logger.SetStdOut(os.Stderr)
		logger.Error("Failed to create or open a database: " + err.Error())
		return
	}

	if t.DB.Existed() {
		logger.Info("Opens existing database: " + colors.Blue("data/havoc.db"))
	} else {
		logger.Info("Creates new database: " + colors.Blue("data/havoc.db"))
	}

	// 获取数据库中的Listener数量
	ListenerCount = t.DB.ListenerCount()

	/* start listeners from the specified yaotl profile */
	// 从Profile中取出Listener
	if t.Profile.Config.Listener != nil {

		/* Start all HTTP/s listeners */
		for _, listener := range t.Profile.Config.Listener.ListenerHTTP {
			var HandlerData = handlers.HTTPConfig{
				Name:         listener.Name,
				Hosts:        listener.Hosts,
				HostBind:     listener.HostBind,
				HostRotation: listener.HostRotation,
				Port:         strconv.Itoa(listener.Port),
				UserAgent:    listener.UserAgent,
				Headers:      listener.Headers,
				Uris:         listener.Uris,
				Secure:       listener.Secure,
			}

			if listener.Cert != nil {
				var Found = true

				if _, err = os.Stat(listener.Cert.Cert); !os.IsNotExist(err) {
					HandlerData.Cert.Cert = listener.Cert.Cert
				} else {
					Found = false
				}

				if _, err = os.Stat(listener.Cert.Key); !os.IsNotExist(err) {
					HandlerData.Cert.Key = listener.Cert.Key
				} else {
					Found = false
				}

				if !Found {
					logger.Error("Failed to find Cert/Key Path for listener '" + listener.Name + "'. Using randomly generated certs")
				}
			}

			if listener.Response != nil {
				HandlerData.Response.Headers = listener.Response.Headers
			}

			if err := t.ListenerStart(handlers.LISTENER_HTTP, HandlerData); err != nil {
				logger.Error("Failed to start listener from profile: " + err.Error())
				return
			}
		}

		/* Start all SMB listeners */
		for _, listener := range t.Profile.Config.Listener.ListenerSMB {
			var HandlerData = handlers.SMBConfig{
				Name:     listener.Name,
				PipeName: listener.PipeName,
			}

			if err := t.ListenerStart(handlers.LISTENER_PIVOT_SMB, HandlerData); err != nil {
				logger.Error("Failed to start listener from profile: " + err.Error())
				return
			}
		}

		/* Start all ExternalC2 listeners */
		for _, listener := range t.Profile.Config.Listener.ListenerExternal {
			var HandlerData = handlers.ExternalConfig{
				Name:     listener.Name,
				Endpoint: listener.Endpoint,
			}

			if err := t.ListenerStart(handlers.LISTENER_EXTERNAL, HandlerData); err != nil {
				logger.Error("Failed to start listener from profile: " + err.Error())
				return
			}
		}

	}

	// t.Listeners的值在上面的t.ListenerStart中添加
	// 输出上次启动过的Listener数量，根据数据库与当前启动的Listener的名字做对比得出
	if ListenerCount > 0 {

		var TotalCount = 0

		if DbName := t.DB.ListenerNames(); len(DbName) > 0 {

			TotalCount = ListenerCount

			for _, name := range DbName {

				for _, listener := range t.Listeners {

					if listener.Name == name {
						TotalCount--
						break
					}

				}

			}

		}

		if TotalCount > 0 {
			logger.Info(fmt.Sprintf("Starting %v listeners from last session", colors.Green(TotalCount)))
		}
	}

	for _, listener := range t.DB.ListenerAll() {

		switch listener["Protocol"] {

		case handlers.AGENT_HTTP, handlers.AGENT_HTTPS:

			var (
				// Config的信息是map表存储的，后续所有的取值都需要通过断言来完成
				Data        = make(map[string]any)
				HandlerData = handlers.HTTPConfig{
					Name: listener["Name"],
				}
			)

			err = json.Unmarshal([]byte(listener["Config"]), &Data)
			if err != nil {
				logger.Error("Failed to unmarshal json bytes to map: " + err.Error())
				continue
			}

			/* set config of http listener */
			HandlerData.Hosts = strings.Split(Data["Hosts"].(string), ", ")
			HandlerData.HostBind = Data["HostBind"].(string)
			HandlerData.HostRotation = Data["HostRotation"].(string)
			HandlerData.Port = Data["Port"].(string)
			HandlerData.UserAgent = Data["UserAgent"].(string)
			HandlerData.Headers = strings.Split(Data["Headers"].(string), ", ")
			HandlerData.Uris = strings.Split(Data["Uris"].(string), ", ")

			HandlerData.Secure = false
			if Data["Secure"].(string) == "true" {
				HandlerData.Secure = true
			}

			if Data["Response Headers"] != nil {

				switch Data["Response Headers"].(type) {

				case string:
					HandlerData.Response.Headers = strings.Split(Data["Response Headers"].(string), ", ")
					break

				default:
					for _, s := range Data["Response Headers"].([]interface{}) {
						HandlerData.Response.Headers = append(HandlerData.Response.Headers, s.(string))
					}

				}
			}

			/* also ignore if we already have a listener running */
			// 是否存在一种情况，端口被占用了，而不是listener已经存在了。。。
			if err := t.ListenerStart(handlers.LISTENER_HTTP, HandlerData); err != nil && err.Error() != "listener already exists" {
				logger.SetStdOut(os.Stderr)
				logger.Error("Failed to start listener from db: " + err.Error())
				return
			}

			break

		case handlers.AGENT_EXTERNAL:

			var (
				Data        = make(map[string]any)
				HandlerData = handlers.ExternalConfig{
					Name: listener["Name"],
				}
			)

			err := json.Unmarshal([]byte(listener["Config"]), &Data)
			if err != nil {
				logger.Debug("Failed to unmarshal json bytes to map: " + err.Error())
				continue
			}

			HandlerData.Endpoint = Data["Endpoint"].(string)

			if err := t.ListenerStart(handlers.LISTENER_EXTERNAL, HandlerData); err != nil && err.Error() != "listener already exists" {
				logger.SetStdOut(os.Stderr)
				logger.Error("Failed to start listener from db: " + err.Error())
				return
			}

			break

		case handlers.AGENT_PIVOT_SMB:

			var (
				Data        = make(map[string]any)
				HandlerData = handlers.SMBConfig{
					Name: listener["Name"],
				}
			)

			err := json.Unmarshal([]byte(listener["Config"]), &Data)
			if err != nil {
				logger.Debug("Failed to unmarshal json bytes to map: " + err.Error())
				continue
			}

			HandlerData.PipeName = Data["PipeName"].(string)

			if err := t.ListenerStart(handlers.LISTENER_PIVOT_SMB, HandlerData); err != nil && err.Error() != "listener already exists" {
				logger.SetStdOut(os.Stderr)
				logger.Error("Failed to start listener from db: " + err.Error())
				return
			}

			break

		}

	}

	t.EventAppend(events.SendProfile(t.Profile))

	// This should hold the Teamserver as long as the WebSocket Server is running
	logger.Debug("Wait til the server shutdown")

	// 如果Teamserver的WebSocket Server关闭了，那么就会退出
	<-ServerFinished
}

func (t *Teamserver) handleRequest(id string) {
	_, NewClient, err := t.Clients[id].Connection.ReadMessage()

	if err != nil {
		// 如果已经结束，直接return
		if err != io.EOF {
			logger.Error("Error reading 2:", err.Error())
			// 如果是connection reset by peer，那么就关闭连接
			if strings.Contains(err.Error(), "connection reset by peer") {
				err := t.Clients[id].Connection.Close()
				if err != nil {
					logger.Error("Error while closing Client connection: " + err.Error())
				}
			}
		}
		return
	}

	// CreatePackage是用来Json反序列化的
	pk := t.Clients[id].Packager.CreatePackage(string(NewClient))

	// 判断连接的用户是否存在于Profile中，如果不存在，那么就发送UserDoNotExists事件，然后关闭连接
	if t.Profile != nil {
		var found = false
		// 遍历所有的用户名，如果有一个匹配的，那么就设置found为true
		for _, UserNames := range t.Profile.ListOfUsernames() {
			if UserNames == pk.Head.User {
				found = true
			}
		}
		// 如果没有找到，那么就发送UserDoNotExists事件，用户不存在
		if !found {
			err := t.SendEvent(id, events.UserDoNotExists())
			if err != nil {
				logger.Error("Error while sending package to " + colors.Red(id) + "")
				return
			}
			t.RemoveClient(id)
		}
	}

	// 如果用户已经存在，那么就发送UserAlreadyExits事件，然后关闭连接，因为一个用户只能连接一次
	for i := range t.Clients {
		if t.Clients[i].Username == pk.Head.User {
			err := t.SendEvent(id, events.UserAlreadyExits())
			if err != nil {
				logger.Error("couldn't send event to client "+colors.Yellow(id)+":", err)
			}
			t.RemoveClient(id)
		}
	}

	if !t.ClientAuthenticate(pk) {
		// 如果用户没有认证成功，那么就发送Authenticated事件，然后关闭连接
		if t.Clients[id] == nil {
			return
		}
		logger.Error("Client [User: " + pk.Body.Info["User"].(string) + "] failed to Authenticate! (" + colors.Red(t.Clients[id].GlobalIP) + ")")
		err := t.SendEvent(id, events.Authenticated(false))
		if err != nil {
			logger.Error("client (" + colors.Red(id) + ") error while sending authenticate message: " + colors.Red(err))
		}
		err = t.Clients[id].Connection.Close()
		if err != nil {
			logger.Error("Failed to close client (" + id + ") socket")
		}
		return
	} else {
		if t.Clients[id] == nil {
			return
		}

		logger.Good("User <" + colors.Blue(pk.Body.Info["User"].(string)) + "> " + colors.Green("Authenticated"))

		// 如果认证成功，会将id作为Client的ClientID，But感觉没有意义，id既是key，又是value
		t.Clients[id].Authenticated = true
		t.Clients[id].ClientID = id

		err := t.SendEvent(id, events.Authenticated(true))
		if err != nil {
			logger.Error("client (" + colors.Red(id) + ") error while sending authenticate message:" + colors.Red(err))
		}
	}

	t.Clients[id].Username = pk.Body.Info["User"].(string)
	packageNewUser := events.ChatLog.NewUserConnected(t.Clients[id].Username)
	// t.EventAppend将事件添加到EventQueue中，然后在EventBroadcast中会将事件发送给所有的用户，再将t.EventAppend中的事件发送给新用户
	t.EventAppend(packageNewUser)
	// 向除了自己以外的所有用户发送NewUserConnected事件
	t.EventBroadcast(id, packageNewUser)

	t.SendAllPackagesToNewClient(id)

	// Client连接后，会一直循环，直到Client断开连接，通过t.DispatchEvent来处理事件
	for {
		_, EventPackage, err := t.Clients[id].Connection.ReadMessage()

		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseAbnormalClosure) {
				logger.Warn("User <" + colors.Blue(t.Clients[id].Username) + "> " + colors.Red("Disconnected"))

				t.EventAppend(events.ChatLog.UserDisconnected(t.Clients[id].Username))
				t.RemoveClient(id)

				return
			} else {
				logger.Error("Error reading :", err.Error())
			}

			err := t.Clients[id].Connection.Close()
			if err != nil {
				logger.Error("Socket Error:", err.Error())
			}

			t.EventAppend(events.ChatLog.UserDisconnected(t.Clients[id].Username))
			t.RemoveClient(id)

			return
		}

		pk := t.Clients[id].Packager.CreatePackage(string(EventPackage))
		pk.Head.Time = time.Now().Format("02/01/2006 15:04:05")

		t.EventAppend(pk)
		t.DispatchEvent(pk)
	}
}

func (t *Teamserver) SetProfile(path string) {
	t.Profile = profile.NewProfile()
	logger.LoggerInstance.STDERR = os.Stderr
	err := t.Profile.SetProfile(path, t.Flags.Server.Default)
	if err != nil {
		logger.SetStdOut(os.Stderr)
		logger.Error("Profile error:", colors.Red(err))
		os.Exit(1)
	}
}

func (t *Teamserver) ClientAuthenticate(pk packager.Package) bool {
	if pk.Head.Event == packager.Type.InitConnection.Type {
		if pk.Body.SubEvent == packager.Type.InitConnection.OAuthRequest {
			if t.Profile != nil {
				if t.Profile.Config.Operators != nil {
					var (
						UserPassword string
						UserName     string
					)
					// 遍历所有的用户，如果有一个用户的用户名和密码匹配，那么就返回true
					// 会根据User.Hashed来决定是否需要对密码进行哈希运算
					for _, User := range t.Profile.Config.Operators.Users {
						if User.Name == pk.Head.User {
							logger.Debug("Found User: " + User.Name)
							UserName = User.Name
							if User.Hashed {
								UserPassword = User.Password
								break
							} else {
								var hash = sha3.New256()
								hash.Write([]byte(User.Password))
								UserPassword = hex.EncodeToString(hash.Sum(nil))
								break
							}
						}
					}
					if pk.Body.Info["Password"].(string) == UserPassword {
						logger.Debug("User " + colors.Red(UserName) + " is authenticated")
						return true
					}
					logger.Debug("User is not authenticated...")
					return false
				} else {
					return false
				}
			} else {
				return false
			}
		} else {
			logger.Error("Wrong SubEvent :: " + strconv.Itoa(pk.Body.SubEvent))
		}
	} else {
		logger.Error("Not a Authenticate request")
	}

	logger.Error("Client failed to authenticate with password hash :: " + pk.Body.Info["Password"].(string))
	return false
}

// 广播发送消息，可以排除一个Client
func (t *Teamserver) EventBroadcast(ExceptClient string, pk packager.Package) {
	for ClientID := range t.Clients {
		if ExceptClient != ClientID {
			err := t.SendEvent(ClientID, pk)
			if err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
				logger.Error("SendEvent error: ", colors.Red(err))
			}
		}
	}
}

func (t *Teamserver) EventNewDemon(DemonAgent *agent.Agent) packager.Package {
	return events.Demons.NewDemon(DemonAgent)
}

func (t *Teamserver) EventAgentMark(AgentID, Mark string) {
	var pk = events.Demons.MarkAs(AgentID, Mark)

	t.EventAppend(pk)
	t.EventBroadcast("", pk)
}

func (t *Teamserver) EventListenerError(ListenerName string, Error error) {
	var pk = events.Listener.ListenerError("", ListenerName, Error)

	t.EventAppend(pk)
	t.EventBroadcast("", pk)

	// also remove the listener from the init packages.
	for EventID := range t.EventsList {
		if t.EventsList[EventID].Head.Event == packager.Type.Listener.Type {
			if t.EventsList[EventID].Body.SubEvent == packager.Type.Listener.Add {
				if name, ok := t.EventsList[EventID].Body.Info["Name"]; ok {
					if name == ListenerName {
						t.EventsList[EventID].Body.Info["Status"] = "Offline"
						t.EventsList[EventID].Body.Info["Error"] = Error.Error()
					}
				}
			}
		}
	}
}

// 发送消息，通过id获取到对应的Client对象，然后通过websocket发送二进制消息
func (t *Teamserver) SendEvent(id string, pk packager.Package) error {
	var (
		buffer bytes.Buffer
		err    error
	)

	err = json.NewEncoder(&buffer).Encode(pk)
	if err != nil {
		return err
	}

	if t.Clients[id] != nil {

		t.Clients[id].Mutex.Lock()

		err = t.Clients[id].Connection.WriteMessage(websocket.BinaryMessage, buffer.Bytes())
		if err != nil {
			t.Clients[id].Mutex.Unlock()
			return err
		}

		t.Clients[id].Mutex.Unlock()

	} else {
		return errors.New(fmt.Sprintf("client (%v) doesn't exist anymore", colors.Red(id)))
	}

	return nil
}

// 通过id获取到对应的Client对象，广播UserDisconnected消息，将用户状态设置为离线，然后删除该Client
func (t *Teamserver) RemoveClient(ClientID string) {
	if _, ok := t.Clients[ClientID]; ok {
		var (
			userDisconnected = t.Clients[ClientID].Username
			Authenticated    = t.Clients[ClientID].Authenticated
		)

		if Authenticated {
			t.EventBroadcast(ClientID, events.ChatLog.UserDisconnected(userDisconnected))
			for UserID := range t.Users {
				if userDisconnected == t.Users[UserID].Name {
					t.Users[UserID].Online = false
				}
			}
		}

		delete(t.Clients, ClientID)
	}
}

func (t *Teamserver) EventAppend(event packager.Package) []packager.Package {

	if event.Head.OneTime != "true" {
		t.EventsList = append(t.EventsList, event)
		return append(t.EventsList, event)
	} else {
		logger.Debug("Onetime package. not gonna save: ", event)
	}

	return nil
}

func (t *Teamserver) EventRemove(EventID int) []packager.Package {
	t.EventsList = append(t.EventsList[:EventID], t.EventsList[EventID+1:]...)

	return append(t.EventsList[:EventID], t.EventsList[EventID+1:]...)
}

// 将t.EventsList中的消息发送给新的Client
func (t *Teamserver) SendAllPackagesToNewClient(ClientID string) {
	for _, Package := range t.EventsList {
		err := t.SendEvent(ClientID, Package)
		if err != nil {
			logger.Error("error while sending info to client("+ClientID+"): ", err)
			return
		}
	}
}

// 设置编译所调用的编译器
func (t *Teamserver) FindSystemPackages() bool {
	var err error

	// 是否指定了自定义的编译器，x64、x86、asm
	if t.Profile.Config.Server.Build != nil {

		if len(t.Profile.Config.Server.Build.Compiler64) > 0 {
			if _, err := os.Stat(t.Profile.Config.Server.Build.Compiler64); os.IsNotExist(err) {
				logger.SetStdOut(os.Stderr)
				logger.Error("Compiler x64 path doesn't exist: " + t.Profile.Config.Server.Build.Compiler64)
				return false
			}

			t.Settings.Compiler64 = t.Profile.Config.Server.Build.Compiler64
		} else {
			t.Settings.Compiler64, err = exec.LookPath("x86_64-w64-mingw32-gcc")
			if err != nil {
				logger.SetStdOut(os.Stderr)
				logger.Error("Couldn't find x64 mingw compiler: " + err.Error())
				return false
			}
		}

		if len(t.Profile.Config.Server.Build.Compiler86) > 0 {
			if _, err := os.Stat(t.Profile.Config.Server.Build.Compiler86); os.IsNotExist(err) {
				logger.SetStdOut(os.Stderr)
				logger.Error("Compiler x86 path doesn't exist: " + t.Profile.Config.Server.Build.Compiler86)
				return false
			}

			t.Settings.Compiler32 = t.Profile.Config.Server.Build.Compiler86
		} else {
			t.Settings.Compiler32, err = exec.LookPath("i686-w64-mingw32-gcc")
			if err != nil {
				logger.SetStdOut(os.Stderr)
				logger.Error("Couldn't find x86 mingw compiler: " + err.Error())
				return false
			}
		}

		if len(t.Profile.Config.Server.Build.Nasm) > 0 {
			if _, err := os.Stat(t.Profile.Config.Server.Build.Nasm); os.IsNotExist(err) {
				logger.SetStdOut(os.Stderr)
				logger.Error("Nasm path doesn't exist: " + t.Profile.Config.Server.Build.Nasm)
				return false
			}

			t.Settings.Nasm = t.Profile.Config.Server.Build.Nasm
		} else {
			t.Settings.Nasm, err = exec.LookPath("nasm")
			if err != nil {
				logger.Error("Couldn't find nasm: " + err.Error())
				return false
			}
		}

	} else {
		t.Settings.Compiler64, err = exec.LookPath("x86_64-w64-mingw32-gcc")
		if err != nil {
			logger.SetStdOut(os.Stderr)
			logger.Error("Couldn't find x64 mingw compiler: " + err.Error())
			return false
		}

		t.Settings.Compiler32, err = exec.LookPath("i686-w64-mingw32-gcc")
		if err != nil {
			logger.SetStdOut(os.Stderr)
			logger.Error("Couldn't find x86 mingw compiler: " + err.Error())
			return false
		}

		t.Settings.Nasm, err = exec.LookPath("nasm")
		if err != nil {
			logger.SetStdOut(os.Stderr)
			logger.Error("Couldn't find nasm: " + err.Error())
			return false
		}
	}

	logger.Info(fmt.Sprintf(
		"Build: \n"+
			" - Compiler x64 : %v\n"+
			" - Compiler x86 : %v\n"+
			" - Nasm         : %v",
		colors.Blue(t.Settings.Compiler64),
		colors.Blue(t.Settings.Compiler32),
		colors.Blue(t.Settings.Nasm),
	))

	return true
}

func (t *Teamserver) EndpointAdd(endpoint *Endpoint) bool {
	for _, e := range t.Endpoints {
		if e.Endpoint == endpoint.Endpoint {
			return false
		}
	}

	t.Endpoints = append(t.Endpoints, endpoint)

	return true
}

func (t *Teamserver) EndpointRemove(endpoint string) []*Endpoint {
	for i := range t.Endpoints {
		if t.Endpoints[i].Endpoint == endpoint {
			t.Endpoints = append(t.Endpoints[:i], t.Endpoints[i+1:]...)
			return t.Endpoints
		}
	}

	return t.Endpoints
}
