#!/bin/bash
# Top-left pane of the recorded session: live traceroute through the mesh.
# Runs inside nylon-demo-alice. Waits for the mesh to come up, then hands the
# pane over to mtr.

while ! ping -c 1 -W 1 10.99.0.3 >/dev/null 2>&1; do sleep 0.5; done

exec mtr --displaymode 0 -i 0.5 --no-dns -a 10.99.0.1 10.99.0.3
