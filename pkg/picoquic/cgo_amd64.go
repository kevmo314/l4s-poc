//go:build amd64
// +build amd64

package picoquic

/*
#cgo CFLAGS: -I${SRCDIR}/../../third_party/picoquic -I${SRCDIR}/../../third_party/picoquic/picoquic -I${SRCDIR}/../../third_party/picotls/include
#cgo LDFLAGS: -L${SRCDIR}/../../third_party/picoquic -L${SRCDIR}/../../third_party/picotls -lpicoquic-core -lpicoquic-log -lpicotls-minicrypto -lpicotls-core -lpicotls-openssl -lpicotls-fusion -lssl -lcrypto -lm -lpthread
*/
import "C"
