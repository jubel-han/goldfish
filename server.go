package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/caiyeon/goldfish/config"
	"github.com/caiyeon/goldfish/handlers"
	"github.com/caiyeon/goldfish/vault"
	"github.com/labstack/echo"
	"github.com/labstack/echo/middleware"

	rice "github.com/GeertJohan/go.rice"

	"golang.org/x/crypto/acme/autocert"
)

var (
	devMode       bool
	wrappingToken string
	cfgPath       string
	cfg           *config.Config
	devVaultCh    chan struct{}
	err           error
	printVersion  bool
)

func init() {
	flag.BoolVar(&devMode, "dev", false, "Set to true to save time in development. DO NOT SET TO TRUE IN PRODUCTION!!")
	flag.BoolVar(&printVersion, "version", false, "Display goldfish's version and exit")
	flag.StringVar(&wrappingToken, "token", "", "Token generated from approle (must be wrapped!)")
	flag.StringVar(&cfgPath, "config", "", "The path of the deployment config HCL file")

	// if vault dev core is active, relay shutdown signal
	shutdownCh := make(chan os.Signal, 4)
	signal.Notify(shutdownCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-shutdownCh
		log.Println("\n\n==> Goldfish shutdown triggered")
		if devVaultCh != nil {
			close(devVaultCh)
		}
		time.Sleep(time.Second)
		os.Exit(0)
	}()
}

func main() {
	// if --version, print and exit success
	flag.Parse()
	if printVersion {
		log.Println(versionString)
		os.Exit(0)
	}

	// if dev mode, run a localhost dev vault instance
	if devMode {
		cfg, devVaultCh, wrappingToken, err = config.LoadConfigDev()
		fmt.Println("wrapping token: " + wrappingToken)
	} else {
		cfg, err = config.LoadConfigFile(cfgPath)
	}

	if err != nil {
		panic(err)
	}
	vault.SetConfig(cfg.Vault)

	// if wrapping token is provided, bootstrap goldfish immediately
	if wrappingToken != "" {
		if err := vault.StartGoldfishWrapper(wrappingToken); err != nil {
			panic(err)
		}
	}

	// display welcome message
	if devMode {
		fmt.Printf(devInitString)
	}
	fmt.Printf(versionString + initString)

	// instantiate echo web server
	e := echo.New()
	e.HideBanner = true

	// setup middleware
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())
	e.Use(middleware.BodyLimit("32M"))
	e.Use(middleware.GzipWithConfig(middleware.GzipConfig{
		Level: 5,
	}))

	// unless explicitly disabled, some extra https configurations need to be set
	if !cfg.Listener.Tls_disable {
		// add extra security headers
		e.Use(middleware.SecureWithConfig(middleware.SecureConfig{
			XSSProtection:         "1; mode=block",
			ContentTypeNosniff:    "nosniff",
			XFrameOptions:         "SAMEORIGIN",
			ContentSecurityPolicy: "default-src 'self' blob: 'unsafe-inline' buttons.github.io api.github.com;",
		}))

		// if redirect is set, forward port 80 to port 443
		if cfg.Listener.Tls_autoredirect {
			e.Pre(middleware.HTTPSRedirect())
			go func(c *echo.Echo) {
				e.Logger.Fatal(e.Start(":80"))
			}(e)
		}

		// if cert file and key file are not provided, try using let's encrypt
		if cfg.Listener.Tls_cert_file == "" && cfg.Listener.Tls_key_file == "" {
			e.AutoTLSManager.Cache = autocert.DirCache("/var/www/.cache")
			e.AutoTLSManager.HostPolicy = autocert.HostWhitelist(cfg.Listener.Address)
			e.Use(middleware.HTTPSRedirectWithConfig(middleware.RedirectConfig{
				Code: 301,
			}))
		}
	}

	// for production, static files are packed inside binary
	// for development, npm dev should serve the static files instead
	if !devMode {
		// use rice for static files instead of regular file system
		assetHandler := http.FileServer(rice.MustFindBox("public").HTTPBox())
		e.GET("/", echo.WrapHandler(assetHandler))
		e.GET("/assets/css/*", echo.WrapHandler(http.StripPrefix("/", assetHandler)))
		e.GET("/assets/js/*", echo.WrapHandler(http.StripPrefix("/", assetHandler)))
		e.GET("/assets/fonts/*", echo.WrapHandler(http.StripPrefix("/", assetHandler)))
		e.GET("/assets/img/*", echo.WrapHandler(http.StripPrefix("/", assetHandler)))
	}

	// API routing
	e.GET("/v1/health", handlers.Health())
	e.GET("/v1/vaulthealth", handlers.VaultHealth())
	e.POST("/v1/bootstrap", handlers.Bootstrap())

	e.POST("/v1/login", handlers.Login())
	e.POST("/v1/login/renew-self", handlers.RenewSelf())

	e.GET("/v1/token/accessors", handlers.GetTokenAccessors())
	e.POST("/v1/token/lookup-accessor", handlers.LookupTokenByAccessor())
	e.POST("/v1/token/revoke-accessor", handlers.RevokeTokenByAccessor())
	e.POST("/v1/token/create", handlers.CreateToken())
	e.GET("/v1/token/listroles", handlers.ListRoles())
	e.GET("/v1/token/role", handlers.GetRole())

	e.GET("/v1/userpass/users", handlers.GetUserpassUsers())
	e.POST("/v1/userpass/delete", handlers.DeleteUserpassUser())

	e.GET("/v1/approle/roles", handlers.GetApproleRoles())
	e.POST("/v1/approle/delete", handlers.DeleteApproleRole())

	e.GET("/v1/policy", handlers.GetPolicy())
	e.DELETE("/v1/policy", handlers.DeletePolicy())

	e.GET("/v1/policy/request", handlers.GetPolicyRequest())
	e.POST("/v1/policy/request", handlers.AddPolicyRequest())
	e.POST("/v1/policy/request/update", handlers.UpdatePolicyRequest())
	e.DELETE("/v1/policy/request/:id", handlers.DeletePolicyRequest())

	e.GET("/v1/transit", handlers.TransitInfo())
	e.POST("/v1/transit/encrypt", handlers.EncryptString())
	e.POST("/v1/transit/decrypt", handlers.DecryptString())

	e.GET("/v1/mount", handlers.GetMount())
	e.POST("/v1/mount", handlers.ConfigMount())

	e.GET("/v1/secrets", handlers.GetSecrets())
	e.POST("/v1/secrets", handlers.PostSecrets())
	e.DELETE("/v1/secrets", handlers.DeleteSecrets())

	e.GET("/v1/bulletins", handlers.GetBulletins())

	e.POST("/v1/wrapping/wrap", handlers.WrapHandler())
	e.POST("/v1/wrapping/unwrap", handlers.UnwrapHandler())

	// serving both static folder and API
	if cfg.Listener.Tls_disable {
		// launch http-only listener
		e.Logger.Fatal(e.Start(cfg.Listener.Address))
	} else if cfg.Listener.Tls_cert_file == "" && cfg.Listener.Tls_key_file == "" {
		// if https is enabled, but no cert provided, try let's encrypt
		e.Logger.Fatal(e.StartAutoTLS(":443"))
	} else {
		// launch listener in https
		e.Logger.Fatal(e.StartTLS(
			cfg.Listener.Address,
			cfg.Listener.Tls_cert_file,
			cfg.Listener.Tls_key_file,
		))
	}
}

const versionString = "Goldfish version: v0.6.0-dev"

const devInitString = `

---------------------------------------------------
Starting local vault dev instance...
Your unseal token and root token can be found above
`

const initString = `
Goldfish successfully bootstrapped to vault

  .
  ...             ...
  .........       ......
   ...........   ..........
     .......... ...............
     .............................
      .............................
         ...........................
        ...........................
        ..........................
        ...... ..................
      ......    ...............
     ..        ..      ....
    .                 ..


`
