//go:build !((darwin || linux) && (arm64 || amd64))

package lua

const jitSupported = false

// trigInline reports whether compiled code computes sin and cos itself.
const trigInline = false

func compileJIT(p *prototype, g *globalState, cl *luaClosure, frame []value) (code []byte, offsets []int32, entries []int, kernels int) {
	return nil, nil, nil, 0
}
