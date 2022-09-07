package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"k8s.io/klog/v2"
)

var Version = "dev"

func usage() {
	flag.PrintDefaults()
	os.Exit(2)
}

func main() {
	// Server options
	flag.Usage = usage
	klog.InitFlags(nil)
	flag.Set("logtostderr", "true")
	flag.Set("stderrthreshold", "INFO")
	flag.Set("v", "3")
	// if utils.GetEnv("ENVIRONMENT", "development") == "development" {
	// 	flag.Set("stderrthreshold", "INFO")
	// 	flag.Set("v", "3")
	// }
	version := flag.Bool("version", false, "Display the version")
	flag.Parse()

	if *version {
		fmt.Printf("Natrium server version: %s\n", Version)
		os.Exit(0)
	}

	// Setup database conn
	config := &database.Config{
		Host:     os.Getenv("DB_HOST"),
		Port:     os.Getenv("DB_PORT"),
		Password: os.Getenv("DB_PASS"),
		User:     os.Getenv("DB_USER"),
		SSLMode:  os.Getenv("DB_SSLMODE"),
		DBName:   os.Getenv("DB_NAME"),
	}
	fmt.Println("🏡 Connecting to database...")
	db, err := database.NewConnection(config)
	if err != nil {
		panic(err)
	}

	fmt.Println("🦋 Running database migrations...")
	database.Migrate(db)

	if utils.GetEnv("WORK_URL", "") == "" && utils.GetEnv("BPOW_KEY", "") == "" {
		panic("Either WORK_URL or BPOW_KEY must be set for work generation")
	}

	// Create app
	app := chi.NewRouter()

	// BPoW if applicable
	var bpowClient *gql.BpowClient
	if utils.GetEnv("BPOW_KEY", "") != "" {
		bpowUrl := "https://boompow.banano.cc/graphql"
		if utils.GetEnv("BPOW_URL", "") != "" {
			bpowUrl = utils.GetEnv("BPOW_URL", "")
		}
		bpowClient = gql.NewBpowClient(bpowUrl, utils.GetEnv("BPOW_KEY", ""), false)
	}

	// Setup RPC Client
	nanoRpcUrl := utils.GetEnv("RPC_URL", "http://localhost:7076")
	rpcClient := net.RPCClient{
		Url:        nanoRpcUrl,
		BpowClient: bpowClient,
	}

	// Setup FCM client
	var fcmClient *fcm.Client
	fcmToken := utils.GetEnv("FCM_API_KEY", "")
	if fcmToken != "" {
		svc, err := fcm.NewClient(fcmToken)
		if err != nil {
			klog.Errorf("Error initating FCM client: %v", err)
			os.Exit(1)
		}
		fcmClient = svc
	}

	// Create repository
	fcmRepo := &repository.FcmTokenRepo{
		DB: db,
	}

	// Setup controllers
	pricePrefix := "nano"
	if *bananoMode {
		pricePrefix = "banano"
	}
	hc := controller.HttpController{RPCClient: &rpcClient, BananoMode: *bananoMode, FcmTokenRepo: fcmRepo, FcmClient: fcmClient}

	// Cors middleware
	app.Use(cors.Handler(cors.Options{
		// AllowedOrigins:   []string{"https://foo.com"}, // Use this to allow specific origin hosts
		//AllowedOrigins:   []string{"*"},
		AllowOriginFunc:  func(r *http.Request, origin string) bool { return true },
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: false,
		MaxAge:           300, // Maximum value not ignored by any of major browsers
	}))
	// Rate limiting middleware
	app.Use(httprate.Limit(
		50,            // requests
		1*time.Minute, // per duration
		// an oversimplified example of rate limiting by a custom header
		httprate.WithKeyFuncs(func(r *http.Request) (string, error) {
			return utils.IPAddress(r), nil
		}),
	))

	// HTTP Routes
	app.Post("/api", hc.HandleAction)
	app.Post("/callback", hc.HandleHTTPCallback)

	// Alerts
	app.Route("/alerts", func(r chi.Router) {
		r.Get("/{lang}", func(w http.ResponseWriter, r *http.Request) {
			lang := chi.URLParam(r, "lang")
			activeAlert, err := GetActiveAlert(lang)
			if err != nil {
				controller.ErrInternalServerError(w, r, "Unable to retrieve alerts")
				return
			}
			render.Status(r, http.StatusOK)
			render.JSON(w, r, activeAlert)
		})
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			activeAlert, err := GetActiveAlert("en")
			if err != nil {
				controller.ErrInternalServerError(w, r, "Unable to retrieve alerts")
				return
			}
			render.Status(r, http.StatusOK)
			render.JSON(w, r, activeAlert)
		})
	})

	// Setup WS endpoint
	wsHub := controller.NewHub(*bananoMode, &rpcClient, fcmRepo)
	go wsHub.Run()
	app.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		controller.WebsocketChl(wsHub, w, r)
	})

	var sio *socketio.Server
	if *socketIoServer {
		// Socket.io endpoint is only for natrium.io/donate
		sio = socketio.NewServer(&engineio.Options{
			Transports: []transport.Transport{
				&polling.Transport{
					CheckOrigin: func(r *http.Request) bool {
						return true
					},
				},
				&websocket.Transport{
					CheckOrigin: func(r *http.Request) bool {
						return true
					},
				},
			},
		})
		sio.OnConnect("/", func(s socketio.Conn) error {
			s.SetContext("")
			klog.Infof("socket.io client connected:", s.ID())
			return nil
		})
		go func() {
			if err := sio.Serve(); err != nil {
				klog.Errorf("socketio listen error: %s\n", err)
			}
		}()
		defer sio.Close()

		app.Handle("/socket.io/", sio)
	}

	// Start nano WS client
	callbackChan := make(chan *net.WSCallbackMsg, 100)
	if utils.GetEnv("NODE_WS_URL", "") != "" {
		go net.StartNanoWSClient(utils.GetEnv("NODE_WS_URL", ""), &callbackChan)
	}

	// Read channel to notify clients of blocks of new blocks
	go func() {
		for msg := range callbackChan {
			if msg.Block.Subtype != "send" {
				continue
			}
			callbackMsg := map[string]interface{}{
				"account": msg.Account,
				"block":   msg.Block,
				"hash":    msg.Hash,
				"is_send": "true",
				"amount":  msg.Amount,
			}
			serialized, err := json.Marshal(callbackMsg)
			if err != nil {
				klog.Errorf("Error serializing callback message: %v", err)
				continue
			}

			// See if they are subscribed
			for client, _ := range wsHub.Clients {
				for _, account := range client.Accounts {
					if account == msg.Block.LinkAsAccount {
						client.Hub.BroadcastToClient(client, serialized)
					}
				}
			}

			// for socket.io
			if sio != nil {
				if msg.Block.LinkAsAccount == "nano_1natrium1o3z5519ifou7xii8crpxpk8y65qmkih8e8bpsjri651oza8imdd" && msg.Block.Subtype == "send" && msg.Amount != "" {
					sio.BroadcastToNamespace("/", "donation_event", map[string]interface{}{
						"amount": msg.Amount,
					})
				}
			}
		}
	}()

	// Automatically update connected clients on prices
	s := gocron.NewScheduler(time.UTC)

	s.Every(60).Seconds().Do(func() {
		// BTC and Nano price
		btcPrice, err := database.GetRedisDB().Hget("prices", fmt.Sprintf("coingecko:%s-btc", pricePrefix))
		if err != nil {
			klog.Errorf("Error getting btc price in cron: %v", err)
			return
		}
		btcPriceFloat, err := strconv.ParseFloat(btcPrice, 64)
		if err != nil {
			klog.Errorf("Error parsing btc price in cron: %v", err)
			return
		}
		var nanoPriceFloat float64
		if *bananoMode {
			nanoPriceStr, err := database.GetRedisDB().Hget("prices", fmt.Sprintf("coingecko:%s-nano", pricePrefix))
			if err != nil {
				klog.Errorf("Error getting nano price in cron: %v", err)
				return
			}
			nanoPriceFloat, err = strconv.ParseFloat(nanoPriceStr, 64)
		}
		for client, _ := range wsHub.Clients {
			currency := client.Currency
			curStr, err := database.GetRedisDB().Hget("prices", fmt.Sprintf("coingecko:%s-%s", pricePrefix, strings.ToLower(currency)))
			if err != nil {
				klog.Errorf("Error getting %s price in cron: %v", currency, err)
				continue
			}
			curFloat, err := strconv.ParseFloat(curStr, 64)
			if err != nil {
				klog.Errorf("Error parsing %s price in cron: %v", currency, err)
				continue
			}
			priceMessage := models.PriceMessage{
				Currency: currency,
				Price:    curFloat,
				BtcPrice: btcPriceFloat,
			}
			if *bananoMode {
				priceMessage.NanoPrice = &nanoPriceFloat
			}
			serialized, err := json.Marshal(priceMessage)
			if err != nil {
				klog.Errorf("Error serializing price message: %v", err)
				continue
			}
			client.Hub.BroadcastToClient(client, serialized)
		}
	})
	s.StartAsync()

	http.ListenAndServe(":3000", app)
}
