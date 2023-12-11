package service

import (
	"fmt"
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/ethereum/go-ethereum/common"
	xsync "github.com/puzpuzpuz/xsync/v3"
	"github.com/samber/lo"
	"github.com/shopspring/decimal"
	"github.com/sirupsen/logrus"
	"github.com/stafiprotocol/chainbridge/utils/crypto/secp256k1"
	lsd_network_factory "github.com/stafiprotocol/eth-lsd-relay/bindings/LsdNetworkFactory"
	"github.com/stafiprotocol/eth-lsd-relay/pkg/config"
	"github.com/stafiprotocol/eth-lsd-relay/pkg/connection"
	"github.com/stafiprotocol/eth-lsd-relay/pkg/local_store"
	"github.com/stafiprotocol/eth-lsd-relay/pkg/utils"
)

type ServiceManager struct {
	stop       chan struct{}
	cfg        *config.Config
	connection *connection.CachedConnection
	srvs       *xsync.MapOf[string, *Service]
	localStore *local_store.LocalStore
}

func NewServiceManager(cfg *config.Config, keyPair *secp256k1.Keypair) (*ServiceManager, error) {
	gasLimitDeci, err := decimal.NewFromString(cfg.GasLimit)
	if err != nil {
		return nil, err
	}
	if gasLimitDeci.LessThanOrEqual(decimal.Zero) {
		return nil, fmt.Errorf("gas limit is zero")
	}

	maxGasPriceDeci, err := decimal.NewFromString(cfg.MaxGasPrice)
	if err != nil {
		return nil, err
	}
	if maxGasPriceDeci.LessThanOrEqual(decimal.Zero) {
		return nil, fmt.Errorf("max gas price is zero")
	}

	conn, err := connection.NewConnection(cfg.Eth1Endpoint, cfg.Eth2Endpoint, keyPair,
		gasLimitDeci.BigInt(), maxGasPriceDeci.BigInt())
	if err != nil {
		return nil, err
	}
	cachedConn, err := connection.NewCachedConnection(conn)
	if err != nil {
		return nil, err
	}
	if err = cachedConn.Start(); err != nil {
		return nil, err
	}
	localStore, err := local_store.NewLocalStore("")
	if err != nil {
		return nil, err
	}

	return &ServiceManager{
		stop:       make(chan struct{}),
		cfg:        cfg,
		connection: cachedConn,
		srvs:       xsync.NewMapOf[string, *Service](),
		localStore: localStore,
	}, nil
}

func (m *ServiceManager) Start() error {
	if !m.cfg.RunForEntrustedLsdNetwork {
		if _, err := m.newAndStartServiceFor(m.cfg.Contracts.LsdTokenAddress); err != nil {
			return err
		}
		return nil
	}

	// for entrusted lsd network
	err := retry.Do(m.syncEntrustedLsdTokens, retry.Attempts(utils.RetryLimit))
	if err != nil {
		return err
	}

	utils.SafeGo(m.startSyncService)

	return nil
}

func (m *ServiceManager) Stop() {
	close(m.stop)
	m.srvs.Range(func(key string, value *Service) bool {
		value.Stop()
		return true
	})
	m.connection.Stop()
}

func (m *ServiceManager) startSyncService() {
	logrus.Info("start listening new entrusted lsd token service")

	for {
		select {
		case <-m.stop:
			logrus.Info("sync entrusted lsd token task has stopped")
			return
		default:
			// sync new entrusted lsd tokens
			err := retry.Do(m.syncEntrustedLsdTokens, retry.Attempts(utils.RetryLimit))
			if err != nil {
				logrus.Error("task has stopped")
				utils.ShutdownRequestChannel <- struct{}{}
				return
			}

			time.Sleep(12 * time.Second)
		}
	}
}

func (m *ServiceManager) syncEntrustedLsdTokens() error {
	lsdNetworkFactoryContract, err := lsd_network_factory.NewLsdNetworkFactory(common.Address{}, m.connection.Eth1Client())
	if err != nil {
		return err
	}
	tokens, err := lsdNetworkFactoryContract.GetEntrustedLsdTokens(nil)
	if err != nil {
		return err
	}
	tokenList := lo.Map(tokens, func(token common.Address, _ int) string { return token.String() })

	for _, token := range tokenList {
		if _, exist := m.srvs.Load(token); !exist {
			// add new entrusted lsd token
			if _, err := m.newAndStartServiceFor(token); err != nil {
				return err
			}
		}
	}

	m.srvs.Range(func(token string, srv *Service) bool {
		if !lo.Contains(tokenList, token) {
			// remove entrusted lsd token
			srv.Stop()
			m.srvs.Delete(token)
		}
		return true
	})

	return nil
}

func (m *ServiceManager) newAndStartServiceFor(lsdToken string) (*Service, error) {
	srvConfig := *m.cfg
	srvConfig.Contracts.LsdTokenAddress = lsdToken
	srv, err := NewService(&srvConfig, m.connection, m.localStore)
	if err != nil {
		return nil, fmt.Errorf("new service for lsd token %s err %s", lsdToken, err.Error())
	}
	if err = srv.Start(); err != nil {
		return nil, fmt.Errorf("start service for lsd token %s err %s", lsdToken, err.Error())
	}
	m.srvs.Store(lsdToken, srv)
	return srv, nil
}