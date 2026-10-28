# Flex Reference Lab

Measures how accurate and how stable a FlexRadio is on each of its frequency references (TCXO, external 10 MHz, GPSDO, GNSS), and produces data and a report that engineering can use.

It was spun off from [FMT Assistant](https://github.com/va3mw/FMTAssistant). It reuses that app's FlexRadio client, its WSJT-X `fmt.all` parser and its Go/Wails shell. Instead of relying on WSJT-X FreqCal, it measures the carrier itself from DAX audio.

## How it measures

1. A carrier of known frequency (the **stimulus**) goes into the radio. This can be a lab generator locked to a GPSDO (best), WWV/WWVH or CHU.
2. The app sets the chosen slice to DIGU with RIT off and no DIGU offset. It tunes the slice so the carrier lands as an audio tone, about 1 kHz by default, and opens a DAX RX stream for it.
3. A phase-tracking estimator mixes the tone to baseband and averages it to 100 Hz. Each gate (1 s by default) then gives:
   - the **average frequency**, from the phase advance between gate ends. There is no dead time, so this is the value used for ADEV.
   - a **least-squares frequency**, which is quieter and is shown live.
   - the carrier **SNR**.
4. The radio reads the carrier at `f_r = dial + slice tuning error + tone`. The slice tuning error comes from the radio's own [`slice get_error`](https://github.com/flexradio/smartsdr-api-docs/wiki/TCPIP-slice#get_error) report, in mHz. The DAX sample clock scales with the reference too, so the reference error is exactly **y = f_true / f_r − 1**, reported in ppb.
   - Sign: **+ means the reference runs fast**, so the radio reads everything low.
5. Every gate is written to `samples.csv` as it is measured. Each row holds:
   - reference state, setting and lock
   - GPS satellites tracked and visible, fix, status and `freq_error`
   - every temperature meter the radio reports
   - SNR, level, lost DAX packets, and why a reading isn't valid

With a good lab carrier at 30 dB SNR, the 1 s reading noise is a few mHz. At 10 MHz that's a few tenths of a ppb.

## Reference sweep

The sweep steps the radio through the references you tick, using `radio oscillator tcxo|gpsdo|external`. It can repeat for several cycles (A-B-A-B…) to check repeatability. Each step:

1. **switching**: waits for the radio to report that reference in use and locked (20-minute timeout).
2. **settle**: waits the settle time. These readings are recorded but kept out of the statistics, and they're used to measure settling time.
3. **measure**: records for the dwell time.

At the end the original oscillator setting is restored. If someone retunes the slice, the app logs it, marks those readings invalid and tunes the slice back. Lock changes and reference changes (including auto fallback) are logged as events.

## Analysis

Each run is split into segments: one per sweep step, or a new segment wherever the reference changes. Analysis is per segment and pooled per reference.

- **Offset:**
  - mean (ppb, and Hz at the carrier), σ, median, min/max
  - uncertainty of the mean: the larger of ADEV at the longest τ and the scatter between visits, combined with the stimulus' uncertainty
  - Δ against the best disciplined reference (the baseline). The stimulus error cancels in Δ.
  - PASS / MARGINAL / FAIL against the limits set in Setup. **The default limits are placeholders.**
- **Stability:**
  - overlapping **ADEV** (with approximate error bars), **MDEV**, overlapping **HDEV** (blind to linear drift) and **TDEV**, at τ = 1, 2, 5, 10, 20, 50 … s
  - the dominant noise type at short τ
  - gaps never bias a term: a term is used only if every reading it spans is present
- **Drift:** least-squares slope in ppb/hour, with its standard error and R².
- **Settling:** the time after a switch until a 10-gate running mean stays within the final value's band.
- **Temperature:** the offset's slope against each radio temperature sensor (ppb/°C) and its correlation.
- **Periodic components:** an FFT of the longest gap-free stretch. It finds wobbles such as the PLL modulation of about 3–4 cycles per minute that the API notes describe on the FLEX-6500/6700.
- **Findings:** plain-language observations, such as unlock periods, low SNR, a measurement-noise floor, too few satellites, or an off-air stimulus limiting accuracy.

**Compare** lines up runs: different radios, firmware, days or temperatures. It shows a table plus overlaid ADEV and offset ± U charts, and can be exported.

## Outputs

Each run lives in `Documents\Flex Reference Lab\Runs\<date> <name>\`:

| File | Contents |
|---|---|
| `samples.csv` | one row per gate with every measured and telemetry field |
| `run.json` | test setup, radio identity and options, stimulus, plan, limits, event log |
| `report.html` | self-contained engineering report: findings, tables, SVG charts, method (written by *Engineering report*) |
| `analysis.json` | every computed number, machine-readable |

Comparison reports go to `Documents\Flex Reference Lab\Comparisons\`.

## Other sources

- **Simulator** (Setup): a built-in radio with a TCXO (with tempco and random walk), a GPSDO (with a 17 s wobble) and an external input. It has lock delays and settling transients. Use it for practice, demos, or checking the pipeline.
- **Import fmt.all** (Runs): turns a WSJT-X FreqCal file into a run, given the dial and true carrier. Imported runs have no telemetry, and WSJT-X's own calibration must have been zero.

## Build

```
wails build
```

Tests: `go test ./...`. The real-time simulator sweep takes about 2 minutes; skip it with `-short`.

For development, set `FLEXREFLAB_HOME=<folder>` to keep settings, logs and runs apart from a real install. `FLEXREFLAB_NO_LOCK=1` allows a second instance.

## Hardware status

**Verified on 2026-10-28** on a FLEX-6600M (firmware 4.4.0.42067, GPSDO, with AetherSDR as the GUI station), receiving WWV at 10 MHz:

- discovery, connect, station bind, reference and GPS status
- meters over VITA: supply voltages, fan and `PATEMP`
- `slice get_error`. The reply looks like `+3.814698,-9.536743` (mHz) and changes after a retune, which is why the app reads it again after tuning.
- DAX RX on channel 1: 24 kHz, 0 of 16,874 packets lost
- carrier acquisition, a 2-minute single run through the app engine, and restoring the slice afterwards

The radio's GPS `freq_error` is a fractional value (for example `-3.62e-12`). The app logs it and reports it per segment in ppb.

**Reference sweep, verified the same day** on the same radio (WWV 10 MHz, TCXO then GPSDO, 60 s settle and 3 min measurement per step):

- `radio oscillator tcxo|gpsdo|auto` works. Each switch reported lock within 1 s, and the sweep restored `auto` at the end.
- **Radio quirk:** when only the *setting* changes (gpsdo → auto, with the GPSDO still in use), the radio sends no status to the client that made the change. That connection keeps seeing the old setting, even after `sub radio all`, while a new connection sees the new one. The app therefore records the new setting itself once the radio replies OK.
- Result: TCXO −1450.8 ppb (−14.5 Hz at 10 MHz) relative to the GPSDO (Δ −1456.7 ± 0.9 ppb, with the stimulus error cancelling). `freq_error_ppb` was 0.

**Not yet verified:**

- whether a GNSS-equipped radio reports `state=gpsdo` or `state=gnss`. Setup can force the label either way.
- the external 10 MHz input

## Tools

- `go run ./cmd/probe -radio <nickname|IP> [-secs 60 -carrier 10e6]` prints the radio's state, meters and `get_error`. With `-secs`, it also measures from DAX and then restores the slice. It never switches references.
- `go run ./cmd/reanalyze <run dir>...` rebuilds `report.html` and `analysis.json` from a run's saved data.
- `FLEXREFLAB_LIVE_RADIO=<ip> go test -run TestLiveSingleRun .` runs a 2-minute single run through the app engine against a real radio.

## License

MIT © 2026 Michael VA3MW
