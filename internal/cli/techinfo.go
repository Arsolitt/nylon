package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sort"
	"time"

	"github.com/encodeous/nylon/core"
	"github.com/encodeous/nylon/internal/buildinfo"
	"github.com/encodeous/nylon/protocol"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
)

var techinfoCmd = &cobra.Command{
	Use:     "techinfo",
	Short:   "Collect a technical snapshot (status + neighbour probes) as JSON for bug reports",
	GroupID: "ny",
	Run: func(cmd *cobra.Command, args []string) {
		itf, _ := cmd.Flags().GetString("interface")
		timeout, _ := cmd.Flags().GetDuration("timeout")
		if timeout <= 0 {
			fmt.Fprintln(os.Stderr, "Error: timeout must be positive")
			os.Exit(1)
		}

		resp, err := core.SendIPCRequest(itf, &protocol.IpcRequest{
			Request: &protocol.IpcRequest_Status{Status: &protocol.StatusRequest{}},
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		if !resp.Ok {
			fmt.Fprintln(os.Stderr, "Error:", resp.Error)
			os.Exit(1)
		}
		status := resp.GetStatus()

		neighbours := append([]*protocol.NeighbourInfo(nil), status.GetNeighbours()...)
		sort.Slice(neighbours, func(i, j int) bool {
			return neighbours[i].GetPeerId() < neighbours[j].GetPeerId()
		})

		// Same protojson options as printJSON, so the embedded status payload and
		// probe results marshal exactly like they do in status/probe JSON output.
		m := protojson.MarshalOptions{Indent: "  ", EmitUnpopulated: true}
		statusRaw, _ := m.Marshal(status)

		probes := make(map[string][]json.RawMessage, len(neighbours))
		replied := 0
		for _, neigh := range neighbours {
			peerId := neigh.GetPeerId()
			probeResp, err := core.SendIPCRequest(itf, &protocol.IpcRequest{
				Request: &protocol.IpcRequest_Probe{Probe: &protocol.ProbeRequest{
					PeerId:    peerId,
					TimeoutMs: durationMillis(timeout),
				}},
			})
			var results []*protocol.EndpointProbeResult
			if err == nil && probeResp.Ok {
				results = probeResp.GetProbe().GetResults()
			}
			raws := make([]json.RawMessage, 0, len(results))
			peerReplied := false
			for _, r := range results {
				raw, _ := m.Marshal(r)
				raws = append(raws, raw)
				if r.Status == protocol.EndpointProbeStatus_ENDPOINT_PROBE_REPLIED {
					peerReplied = true
				}
			}
			probes[peerId] = raws
			if peerReplied {
				replied++
			}
		}

		doc := techinfoDoc{
			CollectedAt: time.Now().Format(time.RFC3339),
			CLI: techinfoCLI{
				Version: buildinfo.Version,
				Commit:  buildinfo.Commit,
				GOOS:    runtime.GOOS,
				GOArch:  runtime.GOARCH,
			},
			Status: statusRaw,
			Probes: probes,
		}
		data, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		fmt.Println(string(data))

		fmt.Fprintf(os.Stderr, "techinfo snapshot for node %s: %d neighbours, %d selected routes, %d/%d probes replied\n",
			status.GetNode().GetNodeId(), len(neighbours), len(status.GetRoutes().GetSelected()), replied, len(neighbours))
	},
}

func init() {
	rootCmd.AddCommand(techinfoCmd)
	techinfoCmd.Flags().StringP("interface", "i", "nylon", "Interface name")
	techinfoCmd.Flags().Duration("timeout", time.Second, "Per-probe response timeout")
}

type techinfoDoc struct {
	CollectedAt string                       `json:"collected_at"`
	CLI         techinfoCLI                  `json:"cli"`
	Status      json.RawMessage              `json:"status"`
	Probes      map[string][]json.RawMessage `json:"probes"`
}

type techinfoCLI struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	GOOS    string `json:"goos"`
	GOArch  string `json:"goarch"`
}
