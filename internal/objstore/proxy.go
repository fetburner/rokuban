package objstore

import "fmt"

// NewProxyDelivery は RangeReader を公開し、SignedURL を隠した配信用の置き場を返す。
func NewProxyDelivery(objects Store) (Store, error) {
	rangeReader, ok := objects.(RangeReader)
	if !ok {
		return nil, fmt.Errorf("proxy delivery requires RangeReader")
	}
	return proxyDeliveryStore{Store: objects, RangeReader: rangeReader}, nil
}

type proxyDeliveryStore struct {
	Store
	RangeReader
}
