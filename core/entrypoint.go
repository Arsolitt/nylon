package core

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime/trace"

	"github.com/encodeous/nylon/internal/buildinfo"
	"github.com/encodeous/nylon/internal/logging"
	"github.com/encodeous/nylon/state"
	"github.com/goccy/go-yaml"
)

func setupDebugging(log *slog.Logger, opts state.NylonOptions) (cleanup func()) {
	if opts.DBG_trace {
		f, err := os.Create("trace.out")
		if err != nil {
			log.Error("failed to create trace file", "error", err)
		} else if err := trace.Start(f); err != nil {
			log.Error("failed to start runtime trace", "error", err)
			_ = f.Close()
		} else {
			log.Info("runtime trace started", "path", "trace.out")
			cleanup = func() {
				trace.Stop()
				_ = f.Close()
			}
		}
	}
	if opts.DBG_debug {
		go func() {
			if err := http.ListenAndServe("0.0.0.0:6060", nil); err != nil {
				log.Warn("debug server failed", "error", err)
			}
		}()
	}
	return cleanup
}

func readCentralConfig(centralPath, nodePath string, tunables *state.RouterTunables) (*state.CentralCfg, error) {
	var centralCfg state.CentralCfg

	file, err := os.ReadFile(centralPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		// fallback to using dist from node config

		var nodeCfg state.LocalCfg

		file, err = os.ReadFile(nodePath)
		if err != nil {
			return nil, fmt.Errorf("central.yaml not found and failed to read node.yaml: %w", err)
		}

		err = yaml.Unmarshal(file, &nodeCfg)
		if err != nil {
			return nil, err
		}

		if nodeCfg.Dist == nil {
			return nil, fmt.Errorf("central.yaml not found and node.yaml has no dist config")
		}

		cfg, err := fetchConfig(
			nodeCfg.Dist.Url,
			nodeCfg.Dist.Key,
			tunables.MaxConfigSize,
			state.NewDNSResolver(nodeCfg.DnsResolvers),
		)
		if err != nil {
			return nil, err
		}

		bytes, err := yaml.Marshal(cfg)
		if err != nil {
			return nil, err
		}
		err = os.WriteFile(centralPath, bytes, 0600)
		if err != nil {
			return nil, err
		}

		centralCfg = *cfg
	} else {
		err = yaml.Unmarshal(file, &centralCfg)
		if err != nil {
			return nil, err
		}
	}
	return &centralCfg, nil
}

func readNodeConfig(nodePath string) (*state.LocalCfg, error) {
	var nodeCfg state.LocalCfg
	file, err := os.ReadFile(nodePath)
	if err != nil {
		return nil, err
	}
	err = yaml.Unmarshal(file, &nodeCfg)
	if err != nil {
		return nil, err
	}
	return &nodeCfg, nil
}

func fatal(msg string, err error) {
	fmt.Fprintf(os.Stderr, "Error: %s: %v\n", msg, err)
	os.Exit(1)
}

// Bootstrap provides startup logic in a real environment
func Bootstrap(centralPath, nodePath, logPath string, level slog.Level, opts state.NylonOptions) {
	tunables := state.DefaultRouterTunables()
	centralCfg, err := readCentralConfig(centralPath, nodePath, &tunables)
	if err != nil {
		fatal("failed to read central config", err)
	}
	nodeCfg, err := readNodeConfig(nodePath)
	if err != nil {
		fatal("failed to read node config", err)
	}
	if logPath != "" {
		nodeCfg.LogPath = logPath
	}

	state.ExpandCentralConfig(centralCfg)
	if err = state.CentralConfigValidator(centralCfg); err != nil {
		fatal("invalid central config", err)
	}
	if err = state.NodeConfigValidator(centralCfg, nodeCfg); err != nil {
		fatal("invalid node config", err)
	}

	logger, closer, err := logging.New(logging.Config{
		Component: "nylon",
		Node:      string(nodeCfg.Id),
		Level:     level,
		JSON:      opts.DBG_log_json,
		FilePath:  nodeCfg.LogPath,
	})
	if err != nil {
		fatal("failed to initialize logging", err)
	}
	defer closer()
	if cleanup := setupDebugging(logger, opts); cleanup != nil {
		defer cleanup()
	}

	logger.Info("starting nylon",
		"version", buildinfo.Version,
		"commit", buildinfo.Commit,
		"node", string(nodeCfg.Id),
		"interface", nodeCfg.InterfaceName,
		"central_config", centralPath,
		"node_config", nodePath,
		"observability_addr", nodeCfg.ObservabilityAddr,
		"log_level", level.String(),
	)
	logger.Debug("effective tunables", "tunables", tunables)

	n, err := NewNylon(*centralCfg, *nodeCfg, logger, centralPath, nil, opts, nil)
	if err != nil {
		fatal("failed to initialize nylon", err)
	}
	if err = n.Start(); err != nil {
		fatal("nylon exited with error", err)
	}
}
