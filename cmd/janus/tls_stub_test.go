package main

import "crypto/tls"

// tlsState is a minimal non-nil *tls.ConnectionState, which is all the header
// middleware inspects.
type tlsState = tls.ConnectionState
