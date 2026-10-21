package conf

import (
	"net"
)

type Server struct {
	Addr_ string       `yaml:"addr"`
	Addr  *net.UDPAddr `yaml:"-"`
}

func (s *Server) setDefaults() {}
func (s *Server) validate() []error {
	var errors []error
	addr, err := validateAddr(s.Addr_, true)
	if err != nil {
		errors = append(errors, err)
	}
	s.Addr = addr
	return errors
}
