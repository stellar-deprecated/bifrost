package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/services/bifrost/common"
	"github.com/stellar/go/services/bifrost/database"
	"github.com/stellar/go/services/bifrost/sse"
	"github.com/stellar/go/support/errors"
	"github.com/stellar/go/support/log"
)

func (s *Server) Start() error {
	s.initLogger()
	s.log.Info("Server starting")

	// Register callbacks
	s.BitcoinListener.TransactionHandler = s.onNewBitcoinTransaction
	s.EthereumListener.TransactionHandler = s.onNewEthereumTransaction
	s.StellarAccountConfigurator.OnAccountCreated = s.onStellarAccountCreated
	s.StellarAccountConfigurator.OnAccountCredited = s.onStellarAccountCredited

	err := s.BitcoinListener.Start()
	if err != nil {
		return errors.Wrap(err, "Error starting BitcoinListener")
	}

	err = s.EthereumListener.Start(s.Config.Ethereum.RpcServer)
	if err != nil {
		return errors.Wrap(err, "Error starting EthereumListener")
	}

	err = s.StellarAccountConfigurator.Start()
	if err != nil {
		return errors.Wrap(err, "Error starting StellarAccountConfigurator")
	}

	signalInterrupt := make(chan os.Signal, 1)
	signal.Notify(signalInterrupt, os.Interrupt)

	go s.poolTransactionsQueue()
	go s.startHTTPServer()

	<-signalInterrupt
	s.shutdown()

	return nil
}

func (s *Server) initLogger() {
	s.log = common.CreateLogger("Server")
}

func (s *Server) shutdown() {
	if s.httpServer != nil {
		log.Info("Shutting down HTTP server...")
		ctx, close := context.WithTimeout(context.Background(), 5*time.Second)
		defer close()
		s.httpServer.Shutdown(ctx)
	}
}

func (s *Server) startHTTPServer() {
	s.sseServer = &sse.Server{}

	r := chi.NewRouter()
	if s.Config.UsingProxy {
		r.Use(middleware.RealIP)
	}
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(s.loggerMiddleware)
	r.Get("/events", s.HandlerEvents)
	r.Post("/generate-bitcoin-address", s.HandlerGenerateBitcoinAddress)
	r.Post("/generate-ethereum-address", s.HandlerGenerateEthereumAddress)

	log.WithField("port", s.Config.Port).Info("Starting HTTP server")

	s.httpServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", s.Config.Port),
		Handler: r,
	}

	err := s.httpServer.ListenAndServe()
	if err != nil {
		if err == http.ErrServerClosed {
			log.Info("HTTP server closed")
		} else {
			log.WithField("err", err).Fatal("Cannot start HTTP server")
		}
	}
}

func (s *Server) loggerMiddleware(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		requestLog := s.log.WithFields(log.F{
			"request_id": r.Context().Value(middleware.RequestIDKey),
			"method":     r.Method,
			"uri":        r.RequestURI,
			"ip":         r.RemoteAddr,
		})

		requestLog.Info("HTTP request")

		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		requestStartTime := time.Now()

		next.ServeHTTP(ww, r)

		duration := big.NewRat(
			time.Since(requestStartTime).Nanoseconds(),
			int64(time.Second),
		)

		requestLog.WithFields(log.F{
			"status":         ww.Status(),
			"response_bytes": ww.BytesWritten(),
			"duration":       duration.FloatString(8),
		}).Info("HTTP response")
	}
	return http.HandlerFunc(fn)
}

func (s *Server) HandlerEvents(w http.ResponseWriter, r *http.Request) {
	// Create SSE stream if not exists but only if address exists.
	// This is required to restart a stream after server restart or failure.
	address := r.URL.Query().Get("stream")
	if !s.sseServer.StreamExists(address) {
		var chain database.Chain
		if len(address) > 0 && address[0] == '1' {
			chain = database.ChainBitcoin
		} else {
			chain = database.ChainEthereum
		}

		association, err := s.Database.GetAssociationByChainAddress(chain, address)
		if err != nil {
			log.WithField("err", err).Error("Error getting address association")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		if association != nil {
			s.sseServer.CreateStream(address)
		}
	}

	s.sseServer.HTTPHandler(w, r)
}

func (s *Server) HandlerGenerateBitcoinAddress(w http.ResponseWriter, r *http.Request) {
	s.handlerGenerateAddress(w, r, database.ChainBitcoin)
}

func (s *Server) HandlerGenerateEthereumAddress(w http.ResponseWriter, r *http.Request) {
	s.handlerGenerateAddress(w, r, database.ChainEthereum)
}

func (s *Server) handlerGenerateAddress(w http.ResponseWriter, r *http.Request, chain database.Chain) {
	w.Header().Set("Access-Control-Allow-Origin", "*")

	stellarPublicKey := r.PostFormValue("stellar_public_key")
	_, err := keypair.Parse(stellarPublicKey)
	if err != nil || (err == nil && stellarPublicKey[0] != 'G') {
		log.WithField("stellarPublicKey", stellarPublicKey).Warn("Invalid stellarPublicKey")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	index, err := s.Database.IncrementAddressIndex(chain)
	if err != nil {
		log.WithField("err", err).Error("Error incrementing address index")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	var address string

	switch chain {
	case database.ChainBitcoin:
		address, err = s.BitcoinAddressGenerator.Generate(index)
	case database.ChainEthereum:
		address, err = s.EthereumAddressGenerator.Generate(index)
	default:
		log.WithField("chain", chain).Error("Invalid chain")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err != nil {
		log.WithFields(log.F{"err": err, "index": index}).Error("Error generating address")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = s.Database.CreateAddressAssociation(chain, stellarPublicKey, address, index)
	if err != nil {
		log.WithFields(log.F{
			"err":              err,
			"chain":            chain,
			"index":            index,
			"stellarPublicKey": stellarPublicKey,
			"address":          address,
		}).Error("Error creating address association")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Create SSE stream
	s.sseServer.CreateStream(address)

	response := GenerateAddressResponse{
		Chain:   string(chain),
		Address: address,
	}

	responseBytes, err := json.Marshal(response)
	if err != nil {
		log.WithField("err", err).Error("Error encoding JSON")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Write(responseBytes)
}
