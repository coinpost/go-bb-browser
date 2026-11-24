package store

import (
	"encoding/json"
)

// ParseResponseSummary extracts seq and ok/error from a JSON-RPC response body.
func ParseResponseSummary(resp []byte) (seq uint64, ok bool, errMsg string) {
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp, &env); err != nil {
		return 0, false, "invalid json-rpc response"
	}
	if env.Error != nil {
		msg := env.Error.Message
		if msg == "" {
			msg = "rpc error"
		}
		return 0, false, msg
	}
	if len(env.Result) == 0 || string(env.Result) == "null" {
		return 0, true, ""
	}
	var r struct {
		Seq uint64 `json:"seq"`
	}
	if err := json.Unmarshal(env.Result, &r); err != nil {
		return 0, true, ""
	}
	return r.Seq, true, ""
}
