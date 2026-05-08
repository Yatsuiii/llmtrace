package proxy

import "github.com/Yatsuiii/llmtrace/internal/config"

type Server struct {
	cfg *config.Config
}

func New(cfg *config.Config) *Server {
	return &Server{cfg: cfg}
}
