#!/bin/bash
# Bottom-left pane of the recorded session: continuous ping across the mesh.
# Runs inside nylon-demo-alice.

while ! ping -c 1 -W 1 10.99.0.3 >/dev/null 2>&1; do sleep 0.5; done

echo "── ping alice (10.99.0.1) → charlie (10.99.0.3) ──"
echo ""

exec fping -l -e -q -p 1000 -t 10 10.99.0.3
