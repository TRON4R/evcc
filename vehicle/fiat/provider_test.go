package fiat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cognitoidentity/types"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/request"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const notPluggedIn = "Deep refresh not allowed if the vehicle is not plugged-in"

func testStatus(t *testing.T, age time.Duration, pluggedIn bool, charging, schedules string) StatusResponse {
	t.Helper()

	var res StatusResponse
	js := fmt.Sprintf(`{"timestamp":%d,"evInfo":{"battery":{"plugInStatus":%t,"chargingStatus":%q,"stateOfCharge":71},"schedules":%s}}`,
		time.Now().Add(-age).UnixMilli(), pluggedIn, charging, schedules)
	require.NoError(t, json.Unmarshal([]byte(js), &res))

	return res
}

func forbidden(debugMsg string) func() (ActionResponse, error) {
	return func() (ActionResponse, error) {
		req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/ev", nil)
		resp := &http.Response{StatusCode: http.StatusForbidden, Request: req}
		return ActionResponse{Name: "FORBIDDEN", DebugMsg: debugMsg}, request.NewStatusError(resp)
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func httpResponse(req *http.Request, code int, body string) *http.Response {
	return &http.Response{
		StatusCode: code,
		Request:    req,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// testAPI returns an api with static credentials that sends all requests to the handler
func testAPI(handler func(*http.Request) *http.Response) *API {
	key, secret, token := "key", "secret", "token"
	expiry := time.Now().Add(time.Hour)

	return &API{
		identity: &Identity{
			ctx: context.Background(),
			uid: "uid",
			creds: &types.Credentials{
				AccessKeyId:  &key,
				SecretKey:    &secret,
				SessionToken: &token,
				Expiration:   &expiry,
			},
		},
		Helper: &request.Helper{Client: &http.Client{
			Transport: roundTripper(func(req *http.Request) (*http.Response, error) {
				return handler(req), nil
			}),
		}},
	}
}

func TestStatusDeepRefresh(t *testing.T) {
	pending := func() (ActionResponse, error) {
		return ActionResponse{ResponseStatus: "pending"}, nil
	}

	for _, tc := range []struct {
		name      string
		age       time.Duration
		pluggedIn bool
		noEvInfo  bool
		started   time.Duration // refresh started this long ago
		action    func() (ActionResponse, error)
		refresh   bool // deep refresh requested
		pending   bool // waiting for refresh afterwards
		unplugged bool
		err       error
	}{
		{name: "fresh status", age: time.Minute},
		{name: "expired, not plugged in", age: time.Hour, unplugged: true},
		{name: "expired, plugged in", age: time.Hour, pluggedIn: true, action: pending, refresh: true, pending: true, err: api.ErrMustRetry},
		{name: "expired, rejected since not plugged in", age: time.Hour, pluggedIn: true, action: forbidden(notPluggedIn), refresh: true, unplugged: true},
		{name: "refresh in progress", age: time.Hour, pluggedIn: true, started: time.Second, pending: true, err: api.ErrMustRetry},
		{name: "refresh timeout", age: time.Hour, pluggedIn: true, started: time.Hour, err: api.ErrTimeout},
		{name: "unplugged while refreshing", age: time.Hour, started: time.Second, unplugged: true},
		{name: "missing ev info", age: time.Hour, noEvInfo: true, action: pending, refresh: true, pending: true, err: api.ErrMustRetry},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var refreshed bool

			v := &Provider{
				expiry: 5 * time.Minute,
				action: func(action, cmd string) (ActionResponse, error) {
					refreshed = true
					assert.Equal(t, "ev", action)
					assert.Equal(t, "DEEPREFRESH", cmd)

					if tc.action == nil {
						return ActionResponse{}, errors.New("unexpected deep refresh")
					}
					return tc.action()
				},
			}

			if tc.started > 0 {
				v.refreshTime = time.Now().Add(-tc.started)
			}

			res, err := v.status(func() (StatusResponse, error) {
				res := testStatus(t, tc.age, tc.pluggedIn, "NOT_CHARGING", "[]")
				if tc.noEvInfo {
					res.EvInfo = nil
				}
				return res, nil
			})

			assert.Equal(t, tc.refresh, refreshed, "deep refresh")
			assert.Equal(t, tc.pending, !v.refreshTime.IsZero(), "pending refresh")
			assert.Equal(t, tc.unplugged, res.unplugged, "unplugged")

			if tc.err != nil {
				assert.ErrorIs(t, err, tc.err)
			} else {
				assert.NoError(t, err)
			}

			if !tc.noEvInfo {
				require.NotNil(t, res.EvInfo)
				assert.Equal(t, 71.0, res.EvInfo.Battery.StateOfCharge)
			}
		})
	}
}

func TestStatusError(t *testing.T) {
	expected := errors.New("status failed")

	v := &Provider{
		expiry: 5 * time.Minute,
		action: func(action, cmd string) (ActionResponse, error) {
			return ActionResponse{}, errors.New("unexpected deep refresh")
		},
	}

	_, err := v.status(func() (StatusResponse, error) {
		return StatusResponse{}, expected
	})

	assert.ErrorIs(t, err, expected)
}

func TestDeepRefreshForbidden(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pinDenied bool
		debugMsg  string
		err       error
	}{
		{name: "not plugged in", debugMsg: notPluggedIn, err: errNotPluggedIn},
		{name: "other reason", debugMsg: "Remote command not authorized"},
		{name: "pin authentication", pinDenied: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var actionCalled bool

			fiat := testAPI(func(req *http.Request) *http.Response {
				if strings.HasSuffix(req.URL.Path, "/ignite/pin/authenticate") {
					if tc.pinDenied {
						return httpResponse(req, http.StatusForbidden, `{"name":"FORBIDDEN","message":"Forbidden","debugMsg":"Invalid PIN"}`)
					}
					return httpResponse(req, http.StatusOK, `{"token":"token"}`)
				}

				actionCalled = true
				return httpResponse(req, http.StatusForbidden, fmt.Sprintf(`{"name":"FORBIDDEN","message":"Forbidden","debugMsg":%q}`, tc.debugMsg))
			})

			v := &Provider{
				action: func(action, cmd string) (ActionResponse, error) {
					return fiat.Action("vin", "1234", action, cmd)
				},
			}

			err := v.deepRefresh()
			assert.Equal(t, !tc.pinDenied, actionCalled, "deep refresh endpoint")

			if tc.err != nil {
				assert.ErrorIs(t, err, tc.err)
				return
			}

			assert.NotErrorIs(t, err, errNotPluggedIn)
			se, ok := errors.AsType[*request.StatusError](err)
			require.True(t, ok, "expected status error, got %v", err)
			assert.Equal(t, http.StatusForbidden, se.StatusCode())
		})
	}
}

// unpluggedProvider returns a provider whose expired status claims plugged in and charging, but the deep refresh is rejected
func unpluggedProvider(t *testing.T, schedules string) *Provider {
	v := &Provider{
		expiry: 5 * time.Minute,
		action: func(action, cmd string) (ActionResponse, error) {
			return forbidden(notPluggedIn)()
		},
	}

	v.statusG = func() (StatusResponse, error) {
		return v.status(func() (StatusResponse, error) {
			return testStatus(t, time.Hour, true, "CHARGING", schedules), nil
		})
	}

	return v
}

func TestStatusRejectedRefresh(t *testing.T) {
	v := unpluggedProvider(t, "[]")

	status, err := v.Status()
	require.NoError(t, err)
	assert.Equal(t, api.StatusA, status)

	soc, err := v.Soc()
	require.NoError(t, err)
	assert.Equal(t, 71.0, soc)
}

const chargeSchedules = `[
	{"enableScheduleType":true,"scheduleType":"CHARGE","startTime":"00:00","endTime":"01:00"},
	{"enableScheduleType":false,"scheduleType":"CHARGE"},
	{"enableScheduleType":false,"scheduleType":"CHARGE"}
]`

func TestControllerUnplugged(t *testing.T) {
	var paths []string

	fiat := testAPI(func(req *http.Request) *http.Response {
		paths = append(paths, req.URL.Path)
		if strings.HasSuffix(req.URL.Path, "/ignite/pin/authenticate") {
			return httpResponse(req, http.StatusOK, `{"token":"token"}`)
		}
		return httpResponse(req, http.StatusOK, `{"responseStatus":"pending"}`)
	})

	c := NewController(unpluggedProvider(t, chargeSchedules), fiat, util.NewLogger("fiat-test"), "vin", "1234")

	// outdated schedules are not written back
	assert.ErrorIs(t, c.ChargeEnable(true), api.ErrMustRetry)
	assert.Empty(t, paths)

	// outdated schedules do not suppress the wake up
	require.NoError(t, c.WakeUp())
	require.NotEmpty(t, paths)
	assert.True(t, strings.HasSuffix(paths[len(paths)-1], "/ev/chargenow"))
}

func statusJSON(age time.Duration, pluggedIn bool, charging string) string {
	return fmt.Sprintf(`{"timestamp":%d,"evInfo":{"battery":{"plugInStatus":%t,"chargingStatus":%q,"stateOfCharge":71}}}`,
		time.Now().Add(-age).UnixMilli(), pluggedIn, charging)
}

func TestStatusCache(t *testing.T) {
	var status string
	var statusCalls, otherCalls int

	fiat := testAPI(func(req *http.Request) *http.Response {
		if req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/status") {
			statusCalls++
			return httpResponse(req, http.StatusOK, status)
		}
		otherCalls++
		return httpResponse(req, http.StatusInternalServerError, "")
	})

	v := NewProvider(fiat, "vin", "1234", 5*time.Minute, 15*time.Minute)

	for _, tc := range []struct {
		name   string
		status string
		res    api.ChargeStatus
		calls  []int // status requests after each of two calls
	}{
		// not plugged in: status is re-read without pin on every call
		{"outdated, not plugged in", statusJSON(time.Hour, false, "NOT_CHARGING"), api.StatusA, []int{1, 2}},
		{"recent, not plugged in", statusJSON(30*time.Second, false, "NOT_CHARGING"), api.StatusA, []int{3, 4}},
		// plugged in: status is cached again
		{"recent, plugged in", statusJSON(0, true, "CHARGING"), api.StatusC, []int{5, 5}},
	} {
		status = tc.status

		for _, calls := range tc.calls {
			res, err := v.Status()
			require.NoError(t, err, tc.name)
			assert.Equal(t, tc.res, res, tc.name)
			assert.Equal(t, calls, statusCalls, tc.name)
		}
	}

	assert.Zero(t, otherCalls, "pin or deep refresh requests")
}

func TestStatusRejectedRefreshCached(t *testing.T) {
	var deepRefreshCalls int

	fiat := testAPI(func(req *http.Request) *http.Response {
		switch {
		case strings.HasSuffix(req.URL.Path, "/status"):
			return httpResponse(req, http.StatusOK, statusJSON(time.Hour, true, "CHARGING"))
		case strings.HasSuffix(req.URL.Path, "/ignite/pin/authenticate"):
			return httpResponse(req, http.StatusOK, `{"token":"token"}`)
		default:
			deepRefreshCalls++
			return httpResponse(req, http.StatusForbidden, fmt.Sprintf(`{"name":"FORBIDDEN","message":"Forbidden","debugMsg":%q}`, notPluggedIn))
		}
	})

	v := NewProvider(fiat, "vin", "1234", 5*time.Minute, 15*time.Minute)

	// rejected deep refresh is not retried on every call
	for range 2 {
		res, err := v.Status()
		require.NoError(t, err)
		assert.Equal(t, api.StatusA, res)
	}

	assert.Equal(t, 1, deepRefreshCalls)
}
