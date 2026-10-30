`rich-hems.json` is a synthetic, credential-free collector snapshot using the
field names, units and nullable signal structure in firmware commit
`998620bbfe485561dc6d745091b158a0c01225f6` (OpenDTU-HEMS PR #38).
It is test data, not a record from a real installation.

Schema references: `src/remote/{Service,HemsSnapshot,HemsJson,Snapshots}.cpp`,
`src/SystemSnapshot.cpp`, `src/hems/energy/Remote.cpp`,
`src/hems/optimizer/{Service,Planner}.cpp`, `src/hems/Controller.cpp`, and
`include/diagnostics/HeapSerialize.h` in that firmware revision.
