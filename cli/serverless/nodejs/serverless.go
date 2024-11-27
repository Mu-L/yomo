// Package nodejs provides a ts serverless runtime
package nodejs

import (
	"os"

	"github.com/yomorun/yomo/cli/serverless"
	"github.com/yomorun/yomo/pkg/wrapper"
)

// nodejsServerless will start js program to run serverless functions.
type nodejsServerless struct {
	name       string
	zipperAddr string
	credential string
	wrapper    *NodejsWrapper
}

// Init initializes the nodejs serverless
func (s *nodejsServerless) Init(opts *serverless.Options) error {
	wrapper, err := NewWrapper(opts.Name, opts.Filename)
	if err != nil {
		return err
	}

	s.name = opts.Name
	s.zipperAddr = opts.ZipperAddr
	s.credential = opts.Credential
	s.wrapper = wrapper

	return nil
}

// Build calls wrapper.Build
func (s *nodejsServerless) Build(_ bool) error {
	return s.wrapper.Build(os.Environ())
}

// Run the wrapper.Run
func (s *nodejsServerless) Run(verbose bool) error {
	err := serverless.LoadEnvFile(s.wrapper.workDir)
	if err != nil {
		return err
	}
	env := os.Environ()
	if verbose {
		env = append(env, "YOMO_LOG_LEVEL=debug")
	}
	return wrapper.Run(s.name, s.zipperAddr, s.credential, s.wrapper, env)
}

// Executable shows whether the program needs to be built
func (s *nodejsServerless) Executable() bool {
	return true
}

func init() {
	serverless.Register(&nodejsServerless{}, ".ts")
}