# Examples

## `fixtures/session-faults.ndjson` (+ `.manifest.json`)

A deterministic **simulated** session: 2 symbols, 20 000 events, seed 42, with this fault schedule:

```text
drop@2000:3,dup@5000,reorder@8000,cross@11000,garbage@14000,skew@17000
```

Each line of the capture is one frame as received:

```json
{"recv_ts":1767225600000120,"raw":"{\"type\":\"snapshot\",\"symbol\":\"XYZ\",\"seq\":0,\"ts\":1767225600000000,\"bids\":[[\"100.00\",\"140\"], …],\"asks\":[…]}"}
```

`raw` is the venue dialect (`simvenue.v1`): decimal strings, `buy`/`sell`, size `"0"` removes a
level. `recv_ts` is the local receive time in microseconds (UTC).

The manifest pins the instruments, 39 ground-truth checkpoints taken from the simulator's true book,
and the digest of everything the engine published during recording.

```bash
make replay         # verify: checkpoints + digest
make demo-replay    # watch it in the UI
make fixtures       # regenerate (changes the pinned digest; commit both files together)
```

The digest changes whenever decoding, sequencing, book logic or the wording of published events
changes. That is intended: regenerate deliberately and review the diff of the manifest.
