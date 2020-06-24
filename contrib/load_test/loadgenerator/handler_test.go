// +build !integration

package loadgenerator

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/gorilla/mux"
	"github.com/line/link/contrib/load_test/tests"
	"github.com/line/link/contrib/load_test/tests/mock"
	"github.com/line/link/contrib/load_test/types"
	linktypes "github.com/line/link/types"
	"github.com/stretchr/testify/require"
	vegeta "github.com/tsenart/vegeta/v12/lib"
)

func TestLoadHandler(t *testing.T) {
	// Given Mock Server
	server := mock.NewServer()
	defer server.Close()

	var testCases = []struct {
		testName                    string
		scenario                    string
		expectedGetAccountCallCount int
	}{
		{
			"Query Account",
			types.QueryAccount,
			0,
		},
		{
			"Query Block",
			types.QueryBlock,
			0,
		},
		{
			"Tx Send",
			types.TxSend,
			tests.ExpectedNumTargets,
		},
		{
			"Tx Empty",
			types.TxEmpty,
			tests.ExpectedNumTargets,
		},
		{
			"Tx Token",
			types.TxToken,
			tests.ExpectedNumTargets,
		},
		{
			"Tx Collection",
			types.TxCollection,
			tests.ExpectedNumTargets,
		},
	}

	for _, tc := range testCases {
		t.Logf("Test %s", tc.testName)
		{
			// Given LoadGenerator
			lg := NewLoadGenerator()
			// Given Router
			r := mux.NewRouter()
			RegisterHandlers(lg, r)
			// Given Request
			config := types.Config{
				MsgsPerTxLoadTest: tests.TestMsgsPerTxLoadTest,
				TPS:               tests.TestTPS,
				Duration:          tests.TestDuration,
				RampUpTime:        tests.TestRampUpTime,
				MaxWorkers:        tests.TestMaxWorkers,
				TargetURL:         server.URL,
				ChainID:           tests.TestChainID,
				CoinName:          tests.TestCoinName,
				Testnet:           tests.TestNet,
				Mnemonic:          tests.TestMnemonic,
			}
			res := httptest.NewRecorder()
			request := types.NewLoadRequest(tc.scenario, config, nil)
			body, err := json.Marshal(request)
			require.NoError(t, err)
			req, err := http.NewRequest("POST", "/target/load", bytes.NewBuffer(body))
			require.NoError(t, err)

			// When
			r.ServeHTTP(res, req)

			// Then
			require.Equal(t, http.StatusOK, res.Code)
			require.Equal(t, tests.TestTPS, lg.config.TPS)
			require.Equal(t, tests.TestDuration, lg.config.Duration)
			require.Equal(t, tc.expectedGetAccountCallCount, mock.GetCallCounter(server.URL).QueryAccountCallCount)
			mock.ClearCallCounter(server.URL)
		}
	}
}

func TestLoadHandlerWithInvalidParameters(t *testing.T) {
	// Given Mock Server
	server := mock.NewServer()
	defer server.Close()

	var testCases = []struct {
		testName string
		scenario string
		config   types.Config
		error    string
	}{
		{
			"with empty chain id",
			types.QueryAccount,
			types.Config{
				MsgsPerTxLoadTest: tests.TestMsgsPerTxLoadTest,
				TPS:               tests.TestTPS,
				Duration:          tests.TestDuration,
				RampUpTime:        tests.TestRampUpTime,
				MaxWorkers:        tests.TestMaxWorkers,
				TargetURL:         server.URL,
				ChainID:           "",
				CoinName:          tests.TestCoinName,
				Testnet:           tests.TestNet,
				Mnemonic:          tests.TestMnemonic,
			},
			"Invalid Load Parameter Error: invalid parameter of load handler\n",
		},
		{
			"with invalid tps",
			types.QueryAccount,
			types.Config{
				MsgsPerTxLoadTest: tests.TestMsgsPerTxLoadTest,
				TPS:               -1,
				Duration:          tests.TestDuration,
				RampUpTime:        tests.TestRampUpTime,
				MaxWorkers:        tests.TestMaxWorkers,
				TargetURL:         server.URL,
				ChainID:           tests.TestChainID,
				CoinName:          tests.TestCoinName,
				Testnet:           tests.TestNet,
				Mnemonic:          tests.TestMnemonic,
			},
			"Invalid Load Parameter Error: invalid parameter of load handler\n",
		},
		{
			"with invalid duration",
			types.QueryAccount,
			types.Config{
				MsgsPerTxLoadTest: tests.TestMsgsPerTxLoadTest,
				TPS:               tests.TestTPS,
				Duration:          -1,
				RampUpTime:        tests.TestRampUpTime,
				MaxWorkers:        tests.TestMaxWorkers,
				TargetURL:         server.URL,
				ChainID:           tests.TestChainID,
				CoinName:          tests.TestCoinName,
				Testnet:           tests.TestNet,
				Mnemonic:          tests.TestMnemonic,
			},
			"Invalid Load Parameter Error: invalid parameter of load handler\n",
		},
		{
			"with invalid ramp up time",
			types.QueryAccount,
			types.Config{
				MsgsPerTxLoadTest: tests.TestMsgsPerTxLoadTest,
				TPS:               tests.TestTPS,
				Duration:          tests.TestDuration,
				RampUpTime:        -1,
				MaxWorkers:        tests.TestMaxWorkers,
				TargetURL:         server.URL,
				ChainID:           tests.TestChainID,
				CoinName:          tests.TestCoinName,
				Testnet:           tests.TestNet,
				Mnemonic:          tests.TestMnemonic,
			},
			"Invalid Load Parameter Error: invalid parameter of load handler\n",
		},
		{
			"with invalid mnemonic",
			types.QueryAccount,
			types.Config{
				MsgsPerTxLoadTest: tests.TestMsgsPerTxLoadTest,
				TPS:               tests.TestTPS,
				Duration:          tests.TestDuration,
				RampUpTime:        tests.TestRampUpTime,
				MaxWorkers:        tests.TestMaxWorkers,
				TargetURL:         server.URL,
				ChainID:           tests.TestChainID,
				CoinName:          tests.TestCoinName,
				Testnet:           tests.TestNet,
				Mnemonic:          "invalid mnemonic",
			},
			"Invalid mnemonic\n",
		},
		{
			"with invalid scenario",
			"invalid type",
			types.Config{
				MsgsPerTxLoadTest: tests.TestMsgsPerTxLoadTest,
				TPS:               tests.TestTPS,
				Duration:          tests.TestDuration,
				RampUpTime:        tests.TestRampUpTime,
				MaxWorkers:        tests.TestMaxWorkers,
				TargetURL:         server.URL,
				ChainID:           tests.TestChainID,
				CoinName:          tests.TestCoinName,
				Testnet:           tests.TestNet,
				Mnemonic:          tests.TestMnemonic,
			},
			"Invalid Scenario Error: invalid scenario\n",
		},
	}

	for _, tc := range testCases {
		t.Logf("Test %s", tc.testName)
		{
			// Given LoadGenerator
			sdk.GetConfig().SetBech32PrefixForAccount(linktypes.Bech32PrefixAcc(tests.TestNet), linktypes.Bech32PrefixAccPub(tests.TestNet))
			lg := NewLoadGenerator()
			// Given Router
			r := mux.NewRouter()
			RegisterHandlers(lg, r)
			// Given Request
			res := httptest.NewRecorder()
			request := types.NewLoadRequest(tc.scenario, tc.config, nil)
			body, err := json.Marshal(request)
			require.NoError(t, err)
			req, err := http.NewRequest("POST", "/target/load", bytes.NewBuffer(body))
			require.NoError(t, err)

			// When
			r.ServeHTTP(res, req)

			body, err = ioutil.ReadAll(res.Body)
			require.NoError(t, err)

			// Then
			require.Equal(t, http.StatusBadRequest, res.Code)
			require.Equal(t, tc.error, string(body))
		}
	}
}

func TestFireHandler(t *testing.T) {
	// Given Mock Server
	server := mock.NewServer()
	defer server.Close()
	// Given LoadGenerator
	lg := NewLoadGenerator()
	// Given Router
	r := mux.NewRouter()
	RegisterHandlers(lg, r)
	// Given Config
	config := types.Config{
		MsgsPerTxLoadTest: tests.TestMsgsPerTxLoadTest,
		TPS:               tests.TestTPS,
		Duration:          tests.TestDuration,
		RampUpTime:        tests.TestRampUpTime,
		MaxWorkers:        tests.TestMaxWorkers,
		TargetURL:         server.URL,
		ChainID:           tests.TestChainID,
		CoinName:          tests.TestCoinName,
		Testnet:           tests.TestNet,
		Mnemonic:          tests.TestMnemonic,
	}
	// Load Targets
	res := httptest.NewRecorder()
	request := types.NewLoadRequest(types.TxSend, config, nil)
	body, err := json.Marshal(request)
	require.NoError(t, err)
	req, err := http.NewRequest("POST", "/target/load", bytes.NewBuffer(body))
	require.NoError(t, err)
	r.ServeHTTP(res, req)
	// Given Fire Request
	res = httptest.NewRecorder()
	req, err = http.NewRequest("POST", "/target/fire", nil)
	require.NoError(t, err)
	// Clear Call Counter
	mock.ClearCallCounter(server.URL)

	// When
	r.ServeHTTP(res, req)

	data, err := ioutil.ReadAll(res.Body)
	require.NoError(t, err)

	var results []vegeta.Result
	require.NoError(t, json.Unmarshal(data, &results))

	// Then
	require.Equal(t, http.StatusOK, res.Code)
	require.Equal(t, "LINK v2 load test: ", results[0].Attack)
	require.Equal(t, uint16(http.StatusOK), results[0].Code)
	require.InDelta(t, tests.ExpectedAttackCount, mock.GetCallCounter(server.URL).BroadcastTxCallCount,
		tests.TestMaxAttackDifference)
}
