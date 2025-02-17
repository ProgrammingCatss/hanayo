package main

// about using johnniedoe/contrib/gzip:
// johnniedoe's fork fixes a critical issue for which .String resulted in
// an ERR_DECODING_FAILED. This is an actual pull request on the contrib
// repo, but apparently, gin is dead.

import (
	"encoding/gob"
	"fmt"
	"time"

	"github.com/fatih/structs"
	"github.com/getsentry/raven-go"
	"github.com/gin-gonic/contrib/sessions"
	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"github.com/johnniedoe/contrib/gzip"
	"github.com/thehowl/conf"
	"github.com/thehowl/qsql"
	"gopkg.in/mailgun/mailgun-go.v1"
	"gopkg.in/redis.v5"
	"zxq.co/ripple/agplwarning"
	"github.com/kawatapw/hanayo/routers/pagemappings"
	"github.com/kawatapw/hanayo/services"
	"github.com/kawatapw/hanayo/services/cieca"
	"zxq.co/x/rs"
)

var startTime = time.Now()

var (
	config struct {
		// Essential configuration that must be always checked for every environment.
		ListenTo        string `description:"ip:port from which to take requests."`
		Unix            bool   `description:"Whether ListenTo is an unix socket."`
		DSN             string `description:"MySQL server DSN"`
		RedisEnable     bool
		AvatarURL       string
		BaseURL         string
		API             string
		BanchoAPI       string `description:"Bancho base url (without /api) that hanayo will use to contact bancho"`
		CheesegullAPI   string
		APISecret       string
		Offline         bool `description:"If this is true, files will be served from the local server instead of the CDN."`

		MainRippleFolder string `description:"Folder where all the non-go projects are contained, such as old-frontend, lets, ci-system. Used for changelog."`
		AvatarsFolder    string `description:"location folder of avatars, used for placing the avatars from the avatar change page."`

		ClanAvatarsFolder string `description:"location folder of clan avatars, used for placing the clan avatars from the clan avatar change page."`

		CookieSecret string

		RedisMaxConnections int
		RedisNetwork        string
		RedisAddress        string
		RedisPassword       string

		DiscordServer string

		BaseAPIPublic string

		Production int `description:"This is a fake configuration value. All of the following from now on should only really be set in a production environment."`

		MailgunDomain        string
		MailgunPrivateAPIKey string
		MailgunPublicAPIKey  string
		MailgunFrom          string

		RecaptchaSite    string
		RecaptchaPrivate string

		SentryDSN string

		IP_API string
	}
	configMap map[string]interface{}
	db        *sqlx.DB
	qb        *qsql.DB
	mg        mailgun.Mailgun
	rd        *redis.Client
)

// Services etc
var (
	CSRF services.CSRF
)

func main() {
	err := agplwarning.Warn("ripple", "Hanayo")
	if err != nil {
		fmt.Println(err)
	}

	fmt.Println("hanayo " + version)

	err = conf.Load(&config, "hanayo.conf")
	switch err {
	case nil:
		// carry on
	case conf.ErrNoFile:
		conf.Export(config, "hanayo.conf")
		fmt.Println("The configuration file was not found. We created one for you.")
		return
	default:
		panic(err)
	}

	var configDefaults = map[*string]string{
		&config.ListenTo:         ":45221",
		&config.CookieSecret:     rs.String(46),
		&config.AvatarURL:        "https://a.ripple.moe",
		&config.BaseURL:          "https://ripple.moe",
		&config.BanchoAPI:        "https://c.ripple.moe",
		&config.CheesegullAPI:    "https://storage.ripple.moe/api",
		&config.API:              "http://localhost:40001/api/v1/",
		&config.APISecret:        "Potato",
		&config.IP_API:           "https://ip.zxq.co",
		&config.DiscordServer:    "#",
		&config.MainRippleFolder: "/home/ripple/ripple",
		&config.MailgunFrom:      `"Ripple" <noreply@ripple.moe>`,
	}
	for key, value := range configDefaults {
		if *key == "" {
			*key = value
		}
	}

	// initialise db
	db, err = sqlx.Open("mysql", config.DSN+"?parseTime=true")
	if err != nil {
		panic(err)
	}
	qb = qsql.New(db.DB)
	if err != nil {
		panic(err)
	}

	// initialise mailgun
	mg = mailgun.NewMailgun(
		config.MailgunDomain,
		config.MailgunPrivateAPIKey,
		config.MailgunPublicAPIKey,
	)

	// initialise CSRF service
	CSRF = cieca.NewCSRF()

	if gin.Mode() == gin.DebugMode {
		fmt.Println("Development environment detected. Starting fsnotify on template folder...")
		err := reloader()
		if err != nil {
			fmt.Println(err)
		}
	}

	// initialise redis
	rd = redis.NewClient(&redis.Options{
		Addr:     config.RedisAddress,
		Password: config.RedisPassword,
	})

	// even if it's not release, we say that it's release
	// so that gin doesn't spam
	gin.SetMode(gin.ReleaseMode)

	gobRegisters := []interface{}{
		[]message{},
		errorMessage{},
		infoMessage{},
		neutralMessage{},
		warningMessage{},
		successMessage{},
	}
	for _, el := range gobRegisters {
		gob.Register(el)
	}

	fmt.Println("Importing templates...")
	loadTemplates("")

	fmt.Println("Setting up rate limiter...")
	setUpLimiter()

	fmt.Println("Exporting configuration...")

	conf.Export(config, "hanayo.conf")

	configMap = structs.Map(config)

	fmt.Println("Intialisation:", time.Since(startTime))

	httpLoop()
}

func httpLoop() {
	for {
		e := generateEngine()
		fmt.Println("Listening on", config.ListenTo)
		if !startuato(e) {
			break
		}
	}
}

func generateEngine() *gin.Engine {
	fmt.Println("Starting session system...")
	var store sessions.Store
	if config.RedisMaxConnections != 0 {
		var err error
		store, err = sessions.NewRedisStore(
			config.RedisMaxConnections,
			config.RedisNetwork,
			config.RedisAddress,
			config.RedisPassword,
			[]byte(config.CookieSecret),
		)
		if err != nil {
			fmt.Println(err)
			store = sessions.NewCookieStore([]byte(config.CookieSecret))
		}
	} else {
		store = sessions.NewCookieStore([]byte(config.CookieSecret))
	}

	r := gin.Default()

	// sentry
	if config.SentryDSN != "" {
		ravenClient, err := raven.New(config.SentryDSN)
		if err != nil {
			fmt.Println(err)
		} else {
			r.Use(Recovery(ravenClient, false))
		}
	}

	r.Use(
		gzip.Gzip(gzip.DefaultCompression),
		pagemappings.CheckRedirect,
		sessions.Sessions("session", store),
		sessionInitializer(),
		rateLimiter(false),
	)

	r.Static("/static", "static")
	r.StaticFile("/favicon.ico", "static/favicon.ico")

	r.POST("/login", loginSubmit)
	r.GET("/logout", logout)

	r.GET("/register", register)
	r.POST("/register", registerSubmit)
	r.GET("/register/verify", verifyAccount)
	r.GET("/register/welcome", welcome)

	r.GET("/clans/create", ccreate)
	r.POST("/clans/create", ccreateSubmit)
	r.GET("/c/:cid", clanPage)
	r.POST("/c/:cid", leaveClan)
	r.GET("/clans/invite/:inv", clanInvite)

	r.GET("/u/:user", userProfile)
	r.GET("/b/:bid", beatmapInfo)

	r.POST("/pwreset", passwordReset)
	r.GET("/pwreset/continue", passwordResetContinue)
	r.POST("/pwreset/continue", passwordResetContinueSubmit)

	r.GET("/settings/password", changePassword)
	r.POST("/settings/password", changePasswordSubmit)
	r.POST("/settings/userpage/parse", parseBBCode)
	r.POST("/settings/avatar", avatarSubmit)
	r.POST("/settings/profbackground/:type", profBackground)
	r.POST("/settings/delete/confirm", deleteAccount)

	r.POST("/settings/clansettings", createInvite)
	r.POST("/settings/clansettings/k", clanKick)

	loadSimplePages(r)

	r.NoRoute(notFound)

	return r
}

const alwaysRespondText = `Ooops! Looks like something went really wrong while trying to process your request.
Perhaps report this to a Kawata developer?
Retrying doing again what you were trying to do might work, too.`
