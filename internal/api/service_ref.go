package api

import (
	"fmt"
	"strconv"
	"strings"
)

func parsePositiveInt32(value string) (int32, error) {
	if value == "" || value[0] == '0' {
		return 0, fmt.Errorf("want a positive decimal integer")
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("want a positive decimal integer")
		}
	}
	n, err := strconv.ParseInt(value, 10, 32)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("want a positive 32-bit integer")
	}
	return int32(n), nil
}

func parseNetworkServiceRefs(values []string) ([]int32, []int32, string) {
	networkIDs := make([]int32, 0, len(values))
	serviceIDs := make([]int32, 0, len(values))
	for _, value := range values {
		network, service, ok := strings.Cut(value, ":")
		if !ok || strings.Contains(service, ":") {
			return nil, nil, fmt.Sprintf("invalid service %q (want <networkId>:<serviceId>)", value)
		}
		networkID, err := parsePositiveInt32(network)
		if err != nil {
			return nil, nil, fmt.Sprintf("invalid service %q: networkId %v", value, err)
		}
		serviceID, err := parsePositiveInt32(service)
		if err != nil {
			return nil, nil, fmt.Sprintf("invalid service %q: serviceId %v", value, err)
		}
		networkIDs = append(networkIDs, networkID)
		serviceIDs = append(serviceIDs, serviceID)
	}
	return networkIDs, serviceIDs, ""
}
