package scrapers

import (
	"context"
	"errors"
	"math"
	"math/big"
	"strings"
	"sync"
	"time"

	uniswap "github.com/diadata-org/diadata/internal/pkg/exchange-scrapers/uniswap"

	"github.com/diadata-org/diadata/pkg/dia"
	"github.com/diadata-org/diadata/pkg/dia/helpers"
	"github.com/diadata-org/diadata/pkg/dia/helpers/ethhelper"
	models "github.com/diadata-org/diadata/pkg/model"
	"github.com/diadata-org/diadata/pkg/utils"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

type UniswapHistoryScraper struct {
	WsClient   *ethclient.Client
	RestClient *ethclient.Client
	// signaling channels for session initialization and finishing
	//initDone     chan nothing
	run          bool
	shutdown     chan nothing
	shutdownDone chan nothing
	// error handling; to read error or closed, first acquire read lock
	// only cleanup method should hold write lock
	errorLock sync.RWMutex
	error     error
	closed    bool
	// used to keep track of trading pairs that we subscribed to
	pairScrapers map[string]*UniswapHistoryPairScraper
	exchangeName string
	chanTrades   chan *dia.Trade
	waitTime     int
	genesisBlock uint64
	db           *models.RelDB
}

const (
	// genesisBlockUniswap            = uint64(6625197)
	genesisBlockUniswap            = uint64(10000000)
	filterQueryBlockNums           = 1000
	uniswapHistoryWaitMilliseconds = 500
)

// NewUniswapScraper returns a new UniswapScraper for the given pair
func NewUniswapHistoryScraper(exchange dia.Exchange, scrape bool, relDB *models.RelDB) *UniswapHistoryScraper {
	log.Info("NewUniswapHistoryScraper: ", exchange.Name)
	var wsClient, restClient *ethclient.Client
	var waitTime int
	var genesisBlock uint64
	var err error

	switch exchange.Name {
	case dia.UniswapExchange:
		exchangeFactoryContractAddress = exchange.Contract.Hex()
		restClient, err = ethclient.Dial(utils.Getenv("ETH_URI_REST", restDial))
		if err != nil {
			log.Fatal(err)
		}

		wsClient, err = ethclient.Dial(utils.Getenv("ETH_URI_WS", wsDial))
		if err != nil {
			log.Fatal(err)
		}
		waitTime = uniswapHistoryWaitMilliseconds
		genesisBlock = genesisBlockUniswap
	case dia.SushiSwapExchange:
		exchangeFactoryContractAddress = exchange.Contract.Hex()
		wsClient, err = ethclient.Dial(utils.Getenv("ETH_URI_WS", wsDial))
		if err != nil {
			log.Fatal(err)
		}

		restClient, err = ethclient.Dial(utils.Getenv("ETH_URI_REST", restDial))
		if err != nil {
			log.Fatal(err)
		}
		waitTime = sushiswapWaitMilliseconds
	case dia.PanCakeSwap:
		log.Infoln("Init ws and rest client for BSC chain")
		wsClient, err = ethclient.Dial(utils.Getenv("ETH_URI_WS_BSC", wsDialBSC))
		if err != nil {
			log.Fatal(err)
		}
		restClient, err = ethclient.Dial(utils.Getenv("ETH_URI_REST_BSC", restDialBSC))
		if err != nil {
			log.Fatal(err)
		}
		waitTime = pancakeswapWaitMilliseconds
		exchangeFactoryContractAddress = exchange.Contract.Hex()

	case dia.DfynNetwork:
		log.Infoln("Init ws and rest client for Polygon chain")
		wsClient, err = ethclient.Dial(wsDialPolygon)
		if err != nil {
			log.Fatal(err)
		}
		restClient, err = ethclient.Dial(restDialPolygon)
		if err != nil {
			log.Fatal(err)
		}
		waitTime = uniswapWaitMilliseconds
		exchangeFactoryContractAddress = exchange.Contract.Hex()

	}

	s := &UniswapHistoryScraper{
		shutdown:     make(chan nothing),
		shutdownDone: make(chan nothing),
		pairScrapers: make(map[string]*UniswapHistoryPairScraper),
		exchangeName: exchange.Name,
		error:        nil,
		chanTrades:   make(chan *dia.Trade),
		waitTime:     waitTime,
		genesisBlock: genesisBlock,
		db:           relDB,
	}

	s.WsClient = wsClient
	s.RestClient = restClient
	if scrape {
		go s.mainLoop()
	}
	return s
}

// runs in a goroutine until s is closed
func (s *UniswapHistoryScraper) mainLoop() {

	// Import tokens which appear as base token and we need a quotation for
	var err error
	reversePairs, err = getReverseTokensFromConfig("uniswap/reverse_tokens")
	if err != nil {
		log.Error("error getting tokens for which pairs should be reversed: ", err)
	}

	// wait for all pairs have added into s.PairScrapers
	time.Sleep(4 * time.Second)
	s.run = true

	numPairs, err := s.getNumPairs()
	if err != nil {
		log.Fatal(err)
	}
	log.Info("Found ", numPairs, " pairs")
	log.Info("Found ", len(s.pairScrapers), " pairScrapers")

	if len(s.pairScrapers) == 0 {
		s.error = errors.New("uniswap: No pairs to scrap provided")
		log.Error(s.error.Error())
	}

	latestBlock, err := s.RestClient.BlockByNumber(context.Background(), nil)
	if err != nil {
		log.Error("get current block number: ", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		time.Sleep(time.Duration(s.waitTime) * time.Millisecond)
		log.Infof("sleep for %v milliseconds: ", s.waitTime)
		wg.Add(1)
		go func(index int, w *sync.WaitGroup) {
			defer w.Done()
			s.fetchTrades(index, s.genesisBlock, latestBlock.NumberU64())
		}(i, &wg)
	}
	wg.Wait()
}

func (s *UniswapHistoryScraper) fetchTrades(i int, blockInit uint64, blockFinal uint64) {
	var pair UniswapPair
	var err error
	if i == -1 && s.exchangeName == "PanCakeSwap" {
		token0 := UniswapToken{
			Address:  common.HexToAddress("0x4DA996C5Fe84755C80e108cf96Fe705174c5e36A"),
			Symbol:   "WOW",
			Decimals: uint8(18),
		}
		token1 := UniswapToken{
			Address:  common.HexToAddress("0xe9e7CEA3DedcA5984780Bafc599bD69ADd087D56"),
			Symbol:   "BUSD",
			Decimals: uint8(18),
		}
		pair = UniswapPair{
			Token0:      token0,
			Token1:      token1,
			ForeignName: "WOW-BUSD",
			Address:     common.HexToAddress("0xA99b9bCC6a196397DA87FA811aEd293B1b488f44"),
		}
	} else {
		pair, err = s.GetPairByID(int64(i))
		if err != nil {
			log.Error("error fetching pair: ", err)
		}
	}
	if len(pair.Token0.Symbol) < 2 || len(pair.Token1.Symbol) < 2 {
		log.Info("skip pair: ", pair.ForeignName)
		return
	}
	if helpers.SymbolIsBlackListed(pair.Token0.Symbol) || helpers.SymbolIsBlackListed(pair.Token1.Symbol) {
		if helpers.SymbolIsBlackListed(pair.Token0.Symbol) {
			log.Infof("skip pair %s. symbol %s is blacklisted", pair.ForeignName, pair.Token0.Symbol)
		} else {
			log.Infof("skip pair %s. symbol %s is blacklisted", pair.ForeignName, pair.Token1.Symbol)
		}
		return
	}
	if helpers.AddressIsBlacklisted(pair.Token0.Address) || helpers.AddressIsBlacklisted(pair.Token1.Address) {
		log.Info("skip pair ", pair.ForeignName, ", address is blacklisted")
		return
	}
	pair.normalizeUniPair()
	ps, ok := s.pairScrapers[pair.ForeignName]
	if ok {
		log.Info(i, ": found pair scraper for: ", pair.ForeignName, " with address ", pair.Address.Hex())
		startblock := blockInit
		endblock := startblock + uint64(filterQueryBlockNums)
		for startblock <= blockFinal {
			log.Info("final Block: ", blockFinal)
			swapsIter, err := s.GetSwapsIterator(pair.Address, startblock, endblock)
			if err != nil {
				if strings.Contains(err.Error(), "query returned more than 10000 results") || strings.Contains(err.Error(), "Log response size exceeded") {
					log.Info("Got `query returned more than 10000 results` error, reduce the window size and try again...")
					endblock = startblock + (endblock-startblock)/2
					continue
				}
				log.Error("get swaps Iterator: ", err.Error())
				time.Sleep(5 * time.Second)
				continue
			}

			for swapsIter.Next() {
				rawSwap := swapsIter.Event
				if ok {
					swap, err := s.normalizeUniswapSwap(*rawSwap)
					if err != nil {
						log.Error("error normalizing swap: ", err)
					}
					price, volume := getSwapData(swap)
					token0 := dia.Asset{
						Address:    pair.Token0.Address.Hex(),
						Symbol:     pair.Token0.Symbol,
						Name:       pair.Token0.Name,
						Decimals:   pair.Token0.Decimals,
						Blockchain: dia.ETHEREUM,
					}
					token1 := dia.Asset{
						Address:    pair.Token1.Address.Hex(),
						Symbol:     pair.Token1.Symbol,
						Name:       pair.Token1.Name,
						Decimals:   pair.Token1.Decimals,
						Blockchain: dia.ETHEREUM,
					}
					t := &dia.Trade{
						Symbol:         ps.pair.Symbol,
						Pair:           ps.pair.ForeignName,
						Price:          price,
						Volume:         volume,
						BaseToken:      token1,
						QuoteToken:     token0,
						Time:           time.Unix(swap.Timestamp, 0),
						ForeignTradeID: swap.ID,
						Source:         s.exchangeName,
						VerifiedPair:   true,
					}
					// If we need quotation of a base token, reverse pair
					if utils.Contains(reversePairs, pair.Token1.Address.Hex()) {
						tSwapped, err := dia.SwapTrade(*t)
						if err == nil {
							t = &tSwapped
						}
					}
					if price > 0 {
						log.Infof("Got trade at time %v - symbol: %s, pair: %s, price: %v, volume:%v", t.Time, t.Symbol, t.Pair, t.Price, t.Volume)
						ps.parent.chanTrades <- t
					}
					if price == 0 {
						log.Info("Got zero trade: ", t)
					}
				}
			}
			startblock = endblock
			endblock = startblock + filterQueryBlockNums
		}
	} else {
		log.Info("Skipping pair due to no pairScraper being available")
	}
}

// GetSwapsIterator returns a channel for swaps of the pair with address @pairAddress
func (s *UniswapHistoryScraper) GetSwapsIterator(pairAddress common.Address, startblock uint64, endblock uint64) (*uniswap.UniswapV2PairSwapIterator, error) {
	log.Infof("get swaps iterator from %v to %v: ", startblock, endblock)
	var pairFiltererContract *uniswap.UniswapV2PairFilterer
	pairFiltererContract, err := uniswap.NewUniswapV2PairFilterer(pairAddress, s.RestClient)
	if err != nil {
		log.Fatal(err)
	}

	swapIterator, err := pairFiltererContract.FilterSwap(
		&bind.FilterOpts{
			Start: startblock,
			End:   &endblock,
		},
		nil,
		nil,
	)
	if err != nil {
		return swapIterator, err
	}

	return swapIterator, nil

}

// normalizeUniswapSwap takes a swap as returned by the swap contract's channel and converts it to a UniswapSwap type
func (s *UniswapHistoryScraper) normalizeUniswapSwap(swap uniswap.UniswapV2PairSwap) (normalizedSwap UniswapSwap, err error) {

	pair, err := s.GetPairByAddress(swap.Raw.Address)
	if err != nil {
		log.Error("error getting pair by address: ", err)
		return
	}
	decimals0 := int(pair.Token0.Decimals)
	decimals1 := int(pair.Token1.Decimals)
	amount0In, _ := new(big.Float).Quo(big.NewFloat(0).SetInt(swap.Amount0In), new(big.Float).SetFloat64(math.Pow10(decimals0))).Float64()
	amount0Out, _ := new(big.Float).Quo(big.NewFloat(0).SetInt(swap.Amount0Out), new(big.Float).SetFloat64(math.Pow10(decimals0))).Float64()
	amount1In, _ := new(big.Float).Quo(big.NewFloat(0).SetInt(swap.Amount1In), new(big.Float).SetFloat64(math.Pow10(decimals1))).Float64()
	amount1Out, _ := new(big.Float).Quo(big.NewFloat(0).SetInt(swap.Amount1Out), new(big.Float).SetFloat64(math.Pow10(decimals1))).Float64()

	blockdata, err := ethhelper.GetBlockData(int64(swap.Raw.BlockNumber), s.db, s.RestClient)
	if err != nil {
		return
	}

	normalizedSwap = UniswapSwap{
		ID:         swap.Raw.TxHash.Hex(),
		Timestamp:  int64(blockdata.Data["Time"].(uint64)),
		Pair:       pair,
		Amount0In:  amount0In,
		Amount0Out: amount0Out,
		Amount1In:  amount1In,
		Amount1Out: amount1Out,
	}
	return
}

// FetchAvailablePairs returns a list with all available trade pairs as dia.ExchangePair for the pairDiscorvery service
func (s *UniswapHistoryScraper) FetchAvailablePairs() (pairs []dia.ExchangePair, err error) {
	time.Sleep(100 * time.Millisecond)
	uniPairs, err := s.GetAllPairs()
	if err != nil {
		return
	}
	for _, pair := range uniPairs {
		if !pair.pairHealthCheck() {
			continue
		}
		quotetoken := dia.Asset{
			Symbol:     pair.Token0.Symbol,
			Name:       pair.Token0.Name,
			Address:    pair.Token0.Address.Hex(),
			Decimals:   pair.Token0.Decimals,
			Blockchain: dia.ETHEREUM,
		}
		basetoken := dia.Asset{
			Symbol:     pair.Token1.Symbol,
			Name:       pair.Token1.Name,
			Address:    pair.Token1.Address.Hex(),
			Decimals:   pair.Token1.Decimals,
			Blockchain: dia.ETHEREUM,
		}
		pairToNormalise := dia.ExchangePair{
			Symbol:         pair.Token0.Symbol,
			ForeignName:    pair.ForeignName,
			Exchange:       "UniswapV2",
			Verified:       true,
			UnderlyingPair: dia.Pair{BaseToken: basetoken, QuoteToken: quotetoken},
		}
		normalizedPair, _ := s.NormalizePair(pairToNormalise)
		pairs = append(pairs, normalizedPair)
	}

	return
}

// FillSymbolData is not used by DEX scrapers.
func (s *UniswapHistoryScraper) FillSymbolData(symbol string) (dia.Asset, error) {
	return dia.Asset{}, nil
}

// GetAllPairs is similar to FetchAvailablePairs. But instead of dia.ExchangePairs it returns all pairs as UniswapPairs,
// i.e. including the pair's address
func (s *UniswapHistoryScraper) GetAllPairs() ([]UniswapPair, error) {
	time.Sleep(20 * time.Millisecond)
	connection := s.RestClient
	var contract *uniswap.IUniswapV2FactoryCaller
	contract, err := uniswap.NewIUniswapV2FactoryCaller(common.HexToAddress(exchangeFactoryContractAddress), connection)
	if err != nil {
		log.Error(err)
	}

	numPairs, err := contract.AllPairsLength(&bind.CallOpts{})
	if err != nil {
		return []UniswapPair{}, err
	}
	wg := sync.WaitGroup{}
	defer wg.Wait()
	pairs := make([]UniswapPair, int(numPairs.Int64()))
	for i := 0; i < int(numPairs.Int64()); i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			uniPair, err := s.GetPairByID(int64(index))
			if err != nil {
				log.Error("error retrieving pair by ID: ", err)
				return
			}
			uniPair.normalizeUniPair()
			pairs[index] = uniPair
		}(i)
	}
	return pairs, nil
}

func (up *UniswapHistoryScraper) NormalizePair(pair dia.ExchangePair) (dia.ExchangePair, error) {
	return pair, nil
}

// GetPairByID returns the UniswapPair with the integer id @num
func (s *UniswapHistoryScraper) GetPairByID(num int64) (UniswapPair, error) {
	log.Info("Get pair ID: ", num)
	var contract *uniswap.IUniswapV2FactoryCaller
	contract, err := uniswap.NewIUniswapV2FactoryCaller(common.HexToAddress(exchangeFactoryContractAddress), s.RestClient)
	if err != nil {
		log.Error(err)
		return UniswapPair{}, err
	}
	numToken := big.NewInt(num)
	pairAddress, err := contract.AllPairs(&bind.CallOpts{}, numToken)
	if err != nil {
		log.Error(err)
		return UniswapPair{}, err
	}

	pair, err := s.GetPairByAddress(pairAddress)
	if err != nil {
		log.Error(err)
		return UniswapPair{}, err
	}
	return pair, err
}

// GetPairByAddress returns the UniswapPair with pair address @pairAddress
func (s *UniswapHistoryScraper) GetPairByAddress(pairAddress common.Address) (pair UniswapPair, err error) {
	connection := s.RestClient
	var pairContract *uniswap.IUniswapV2PairCaller
	pairContract, err = uniswap.NewIUniswapV2PairCaller(pairAddress, connection)
	if err != nil {
		log.Error(err)
		return UniswapPair{}, err
	}

	// Getting tokens from pair ---------------------
	address0, _ := pairContract.Token0(&bind.CallOpts{})
	address1, _ := pairContract.Token1(&bind.CallOpts{})
	var token0Contract *uniswap.IERC20Caller
	var token1Contract *uniswap.IERC20Caller
	token0Contract, err = uniswap.NewIERC20Caller(address0, connection)
	if err != nil {
		log.Error(err)
	}
	token1Contract, err = uniswap.NewIERC20Caller(address1, connection)
	if err != nil {
		log.Error(err)
	}
	symbol0, err := token0Contract.Symbol(&bind.CallOpts{})
	if err != nil {
		log.Error(err)
	}
	symbol1, err := token1Contract.Symbol(&bind.CallOpts{})
	if err != nil {
		log.Error(err)
	}
	decimals0, err := s.GetDecimals(address0)
	if err != nil {
		log.Error(err)
		return UniswapPair{}, err
	}
	decimals1, err := s.GetDecimals(address1)
	if err != nil {
		log.Error(err)
		return UniswapPair{}, err
	}

	name0, err := s.GetName(address0)
	if err != nil {
		log.Error(err)
		return UniswapPair{}, err
	}
	name1, err := s.GetName(address1)
	if err != nil {
		log.Error(err)
		return UniswapPair{}, err
	}
	token0 := UniswapToken{
		Address:  address0,
		Symbol:   symbol0,
		Decimals: decimals0,
		Name:     name0,
	}
	token1 := UniswapToken{
		Address:  address1,
		Symbol:   symbol1,
		Decimals: decimals1,
		Name:     name1,
	}
	foreignName := symbol0 + "-" + symbol1
	pair = UniswapPair{
		ForeignName: foreignName,
		Address:     pairAddress,
		Token0:      token0,
		Token1:      token1,
	}
	return pair, nil
}

// GetDecimals returns the decimals of the token with address @tokenAddress
func (s *UniswapHistoryScraper) GetDecimals(tokenAddress common.Address) (decimals uint8, err error) {

	var contract *uniswap.IERC20Caller
	contract, err = uniswap.NewIERC20Caller(tokenAddress, s.RestClient)
	if err != nil {
		log.Error(err)
		return
	}
	decimals, err = contract.Decimals(&bind.CallOpts{})

	return
}

func (s *UniswapHistoryScraper) GetName(tokenAddress common.Address) (name string, err error) {

	var contract *uniswap.IERC20Caller
	contract, err = uniswap.NewIERC20Caller(tokenAddress, s.RestClient)
	if err != nil {
		log.Error(err)
		return
	}
	name, err = contract.Name(&bind.CallOpts{})

	return
}

// getNumPairs returns the number of available pairs on Uniswap
func (s *UniswapHistoryScraper) getNumPairs() (int, error) {

	var contract *uniswap.IUniswapV2FactoryCaller
	contract, err := uniswap.NewIUniswapV2FactoryCaller(common.HexToAddress(exchangeFactoryContractAddress), s.RestClient)
	if err != nil {
		log.Error(err)
	}

	// Getting pairs ---------------
	numPairs, err := contract.AllPairsLength(&bind.CallOpts{})
	return int(numPairs.Int64()), err
}

// Close closes any existing API connections, as well as channels of
// PairScrapers from calls to ScrapePair
func (s *UniswapHistoryScraper) Close() error {
	if s.closed {
		return errors.New("UniswapScraper: Already closed")
	}
	s.WsClient.Close()
	s.RestClient.Close()
	close(s.shutdown)
	<-s.shutdownDone
	s.errorLock.RLock()
	defer s.errorLock.RUnlock()
	return s.error
}

// ScrapePair returns a PairScraper that can be used to get trades for a single pair from
// this APIScraper
func (s *UniswapHistoryScraper) ScrapePair(pair dia.ExchangePair) (PairScraper, error) {
	s.errorLock.RLock()
	defer s.errorLock.RUnlock()
	if s.error != nil {
		return nil, s.error
	}
	if s.closed {
		return nil, errors.New("UniswapScraper: Call ScrapePair on closed scraper")
	}
	ps := &UniswapHistoryPairScraper{
		parent: s,
		pair:   pair,
	}
	s.pairScrapers[pair.ForeignName] = ps
	return ps, nil
}

// UniswapPairScraper implements PairScraper for Uniswap
type UniswapHistoryPairScraper struct {
	parent *UniswapHistoryScraper
	pair   dia.ExchangePair
	closed bool
}

// Close stops listening for trades of the pair associated with s
func (ps *UniswapHistoryPairScraper) Close() error {
	ps.closed = true
	return nil
}

// Channel returns a channel that can be used to receive trades
func (ps *UniswapHistoryScraper) Channel() chan *dia.Trade {
	return ps.chanTrades
}

// Error returns an error when the channel Channel() is closed
// and nil otherwise
func (ps *UniswapHistoryPairScraper) Error() error {
	s := ps.parent
	s.errorLock.RLock()
	defer s.errorLock.RUnlock()
	return s.error
}

// Pair returns the pair this scraper is subscribed to
func (ps *UniswapHistoryPairScraper) Pair() dia.ExchangePair {
	return ps.pair
}