package p2p

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

func RegisterWithSentinel(APIKey, validatorAddrHex, peerID, sentinel, validatorPaymentAddr string) {
	fmt.Println("[p2p.sentinel]: Registering with sentinel", APIKey, validatorAddrHex, peerID, sentinel, validatorPaymentAddr)

	params := [4]string{APIKey, validatorAddrHex, peerID, validatorPaymentAddr}
	data := map[string]interface{}{
		"method": "register_peer",
		"params": params,
		"id":     1,
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		fmt.Println("[p2p.sentinel]: Err marshalling json data:", err)
		return
	}

	go func() {
		resp, err := http.Post(sentinel, "application/json", bytes.NewBuffer(jsonData)) //nolint:gosec
		if err != nil {
			fmt.Println("[p2p.sentinel]: Err making post request to sentinel:", err)
		} else {
			fmt.Println("[p2p.sentinel]: Successfully registered with sentinel", resp)
			defer resp.Body.Close()
		}
	}()
}
