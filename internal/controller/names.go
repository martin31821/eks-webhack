package controller

import (
	"fmt"
	"hash/fnv"
)

// CloneServiceName returns the deterministic NodePort clone name for a given Service name.
// The result is always ≤ 63 characters to satisfy RFC 1123.
func CloneServiceName(originalName string) string {
	const suffix = "-whp"
	if len(originalName)+len(suffix) <= 63 {
		return originalName + suffix
	}
	// 50 + 1("-") + 8(hex32) + 4("-whp") = 63
	h := fnv.New32a()
	_, _ = h.Write([]byte(originalName))
	return fmt.Sprintf("%s-%08x-whp", originalName[:50], h.Sum32())
}

// ManagedEndpointSliceName returns the deterministic name for the managed EndpointSlice
// derived from a given source (NodePort clone) EndpointSlice name.
func ManagedEndpointSliceName(sourceEPSName string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(sourceEPSName))
	return fmt.Sprintf("whp-%08x", h.Sum32())
}
