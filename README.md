# Flex Reference Lab

**Flex Reference Lab measures how accurate and how stable a FlexRadio's frequency reference is.** It works on each reference the radio can use: the internal **TCXO**, an **external 10 MHz** input, the **GPSDO** and **GNSS**. It turns the measurements into numbers, charts and a report you can hand to engineering.

It's a Windows desktop app (Go + Wails), spun off from [FMT Assistant](https://github.com/va3mw/FMTAssistant).

---

## Contents

- [What it does, in one paragraph](#what-it-does-in-one-paragraph)
- [What you need](#what-you-need)
- [Quick start](#quick-start)
- [What the app does to your radio (and puts back)](#what-the-app-does-to-your-radio-and-puts-back)
- [How the measurement works](#how-the-measurement-works)
- [Single run and reference sweep](#single-run-and-reference-sweep)
- [What gets analysed](#what-gets-analysed)
- [The engineering report](#the-engineering-report)
- [Comparing runs](#comparing-runs)
- [Files and data format](#files-and-data-format)
- [Choosing a stimulus](#choosing-a-stimulus)
- [Other sources: simulator and WSJT-X import](#other-sources-simulator-and-wsjt-x-import)
- [Command-line tools](#command-line-tools)
- [Limitations and known radio behaviour](#limitations-and-known-radio-behaviour)
- [Hardware tested](#hardware-tested)
- [Building and testing](#building-and-testing)
- [License](#license)

---

## What it does, in one paragraph

You feed the radio a carrier of known frequency: a GPSDO-locked lab generator, or WWV / WWVH off air. The app tunes one of your slices so the carrier becomes an audio tone of about 1 kHz. It takes that audio straight from the radio as a DAX stream and measures the tone's frequency every second, to a few millihertz. From the difference between where the radio *thinks* the carrier is and where it *really* is, it works out the reference's error in **parts per billion (ppb)**.

While it measures, it logs:
- the radio's reference state and lock
- the GPS satellites and the GPS-reported frequency error
- every temperature sensor

It can also step the radio through each of its references on its own (a "sweep"), so they're all measured against the same carrier in one session.

Afterwards it computes:
- offset, scatter and drift
- Allan, modified Allan, Hadamard and time deviation
- settling time after a switch
- temperature coefficient
- periodic wobble
- differences between references

Each reference gets a PASS / FAIL against a limit you set. Everything goes into a self-contained HTML report, with the raw data in CSV and JSON.

---

## What you need

- **Windows 10 or 11.**
- **A FlexRadio 6000- or 8000-series radio** on the same network.
- **A GUI client connected to the radio:** SmartSDR, AetherSDR or Maestro. The API only lets other programs use slices that belong to a GUI station. The app binds to that station and borrows one of its slices.
- **A free DAX channel** (1–8) that nothing else is using during the run.
- **A carrier of known frequency** at the antenna input. See [Choosing a stimulus](#choosing-a-stimulus).

You don't need WSJT-X, SmartSDR DAX or virtual audio cables. The app gets the audio straight from the radio.

---

## Quick start

1. Start **FlexRefLab.exe**.
2. **Setup** tab:
   - Pick your radio from *Discovered radios* (or type its IP) and click **Connect**.
   - Choose the **GUI station**, the **slice** to borrow, and a **DAX channel**.
   - Under **Stimulus**, choose a preset or enter the carrier's true frequency and its uncertainty.
   - Under **Reference sweep plan**, tick the references to compare and set the settle time, measure time and number of cycles.
   - Under **Offset limits**, enter the PASS/FAIL limits. *The defaults are placeholders.*
   - Click **Save settings**.
3. **Measure** tab: name the run, then click **Start single run** or **Start reference sweep**. The app asks you to confirm before it touches the radio.
4. Watch it live:
   - the reference error now and over the last 60 s
   - the carrier's SNR
   - which step and phase the sweep is in, with a countdown
   - a live chart and the latest readings

   You can add notes ("opened the shack door") as you go.
5. **Runs & analysis** tab: click the run to see its findings, tables and charts. Click **Engineering report (HTML)** for the shareable report, or **Open folder** for the CSV and JSON.
6. **Compare** tab: tick several runs to put them side by side.

No radio handy? Tick **Use the built-in simulator** in Setup and try everything.

---

## What the app does to your radio (and puts back)

The app is a *non-GUI* API client. It **never transmits**.

| When | It changes | At the end it restores |
|---|---|---|
| A run starts | The chosen slice: mode → **DIGU**, DIGU offset → 0, RIT → off, frequency → carrier − tone, DAX channel → yours | The slice's original mode, DIGU offset, RIT, DAX channel and frequency |
| A run is running | Opens a DAX RX stream | The stream is removed |
| During a run | If someone retunes the slice, the app logs it, marks those readings invalid and tunes it back | — |
| **Sweep only** | The radio's **reference oscillator** (`radio oscillator tcxo / gpsdo / external`) | The oscillator setting the radio had before the sweep, e.g. `auto` |
| Setup tab, by hand | The oscillator, if you click *Apply*. It asks first. | Nothing; it's your choice |

Restoring happens when a run finishes, when you press Stop, when you disconnect, and when you close the app.

---

## How the measurement works

### The signal path

```
carrier ──► radio (slice in DIGU, dial = carrier − ~1000 Hz) ──► DAX RX audio, 24 kHz
        ──► mix down by an NCO at the tone ──► average 10 ms blocks (100 complex samples/s)
        ──► unwrap the phase ──► one reading per gate (1 s by default)
```

1. **Acquisition.** An FFT of 2 seconds of audio finds the strongest tone within ±300 Hz of where it should be. It needs to be at least 15 dB above the noise. The numerically controlled oscillator (NCO) is then set to it.
2. **Tracking.**
   - The audio is mixed to baseband and averaged in 10 ms blocks. That boxcar exactly nulls the mixing image at twice the tone.
   - The phase of each block is unwrapped.
   - If the tone drifts more than 20 Hz from the NCO, the NCO follows it. A TCXO that is 1.5 ppm off moves a 10 MHz carrier by 15 Hz.
3. **Each gate gives two frequencies:**
   - **Average frequency:** the phase advance from the end of the previous gate to the end of this one, divided by the gate time. Consecutive gates share their end points, so there is no dead time. This is exactly what a frequency counter would read, and it's the right input for the Allan family.
   - **Least-squares frequency:** the slope of a line fitted through the gate's phase. It rejects white phase noise better, so it's shown live and on the offset chart.

   Each gate also records the carrier-to-noise ratio in the 100 Hz tracking bandwidth and the carrier level in dBFS.

### From tone to reference error

The radio believes the carrier is at

```
f_radio = dial + slice tuning error + tone
```

The **slice tuning error** is the radio's own report of how far its digital tuning sits from the requested frequency. The app reads it with [`slice get_error`](https://github.com/flexradio/smartsdr-api-docs/wiki/TCPIP-slice#get_error) (in mHz), and again after every retune because it changes. It has nothing to do with the reference, so it's subtracted.

Every frequency the radio makes scales with its reference. That includes the LO and the DAX sample clock, so the result is exact, not an approximation:

```
y = f_true / f_radio − 1          (reported in ppb = y × 10⁹)
```

**Sign convention: positive means the reference runs fast**, so the radio reads every frequency **low** by that fraction. For example, −1450 ppb at 10 MHz means the radio shows the carrier 14.5 Hz too high.

The app also records the radio's own frequency correction (`freq_error_ppb`), and the GPS receiver's reported `freq_error`, which is a fractional value such as `-3.6e-12`.

### When a reading doesn't count

Each reading is marked **valid** or gets a reason. Readings are kept out of the statistics (but stay in the data) when:

| Reason | Meaning |
|---|---|
| `low SNR` | Below 10 dB in 100 Hz: cycle slips are likely |
| `phase jump` | The phase left its trend by more than 1.5 rad |
| `DAX packets lost` | The VITA-49 packet count skipped |
| `first gate after a break` | No continuous phase to measure from yet |
| `no carrier found` | Acquisition failed |
| `slice retuned` | Someone moved the slice |
| *settle phase* | Inside the settling time after a reference switch (used for settling analysis only) |
| *not locked* | A GPSDO, GNSS or external reference reported it wasn't locked |

---

## Single run and reference sweep

**Single run.** The app measures whatever reference the radio is on until you press Stop. If the radio changes reference by itself during the run (an `auto` fallback, for instance), it logs the change and starts a new segment.

**Reference sweep.** The app steps through the references you ticked, optionally for several cycles (TCXO → GPSDO → TCXO → GPSDO …). Each step goes through three phases:

1. **switching:** the app selects the reference and waits for the radio to report it in use and locked. TCXO counts as locked straight away. It gives up on the step after 20 minutes. If the radio refuses the command, the step is skipped.
2. **settle:** it records for the settle time, but keeps those readings out of the statistics.
3. **measure:** it records for the measure time.

At the end, the original oscillator setting and the slice are restored. Every switch, lock, unlock, retune, note and error goes into the run's event log with a UTC timestamp.

Repeating cycles shows **repeatability**: does each reference return to the same offset? It also lets drift in the setup average out.

---

## What gets analysed

A run is split into **segments**: one per sweep step, or wherever the reference changed. Each segment is analysed alone, then each reference is summarised across all its segments.

### Per segment

| Quantity | What it is |
|---|---|
| **Mean offset** | ppb, and Hz at the carrier |
| **σ, median, min, max, peak-to-peak** | Scatter of the 1 s readings |
| **U (uncertainty of the mean)** | The larger of σ/√N and ADEV at the longest τ. The ADEV term holds even when the noise is correlated, which σ/√N doesn't. |
| **Drift** | Least-squares slope in ppb/hour, with its standard error and R². Only reported as a finding over at least 15 minutes and above 3σ. |
| **Settling time** | Seconds from the switch until a 10-gate running mean last left a band around the final value (3σ of the final quarter, at least 0.5 ppb) |
| **ADEV, MDEV, HDEV, TDEV** | At τ = 1, 2, 5, 10, 20, 50 … s; see below |
| **Noise type** | The dominant noise at short τ, from the ADEV/MDEV slopes: white PM (measurement noise), flicker PM, white FM, flicker FM floor, random-walk FM, drift |
| **Temperature** | For each radio temperature sensor: mean and range, and the offset's slope against it (ppb/°C ± error) with the correlation r |
| **Periodic components** | A detrended, Hann-windowed FFT of the longest gap-free stretch. It gives the strongest period, its amplitude in ppb and its prominence. A wobble such as the PLL modulation of about 3–4 cycles per minute described for the FLEX-6500/6700 shows up here. |
| **Housekeeping** | Readings used, settling, invalid (with reasons) and unlocked; mean and minimum SNR; slice tuning error; GPS satellites tracked (mean and minimum); GPS `freq_error` in ppb |

### Stability (the Allan family)

All four are computed on an even time grid. A term is used **only if every reading it spans is present**, so gaps cost data but never bias the result.

- **ADEV:** overlapping Allan deviation, the standard stability measure. Error bars are approximate (σ/√(N/m)).
- **MDEV:** modified Allan deviation. It tells white phase noise from flicker phase noise. Where MDEV falls as τ^−1.5, what you see is the *measurement's* noise, not the radio's.
- **HDEV:** overlapping Hadamard deviation. It is blind to linear drift, so it shows a drifting TCXO's real noise.
- **TDEV:** time deviation, in nanoseconds.

### Per reference

| Quantity | What it is |
|---|---|
| **Mean offset** | Weighted by readings, across all of the reference's segments |
| **Repeatability** | The scatter of the segment means, when there are two or more visits |
| **U (total)** | The statistical uncertainty, combined with the stimulus' stated uncertainty |
| **Δ vs baseline** | The offset minus that of the best disciplined reference measured (GNSS, then GPSDO, then external). Both use the same carrier, so the **stimulus error cancels**. That makes Δ meaningful even off air. |
| **ADEV at 1, 10, 100 and 1000 s** | Taken from the reference's longest segment |
| **Settling time** | The median over its segments |
| **PASS / MARGINAL / FAIL** | PASS if \|mean\| + U is within the limit. FAIL if \|mean\| − U is outside it. MARGINAL if the limit falls within the uncertainty, meaning you should measure longer or with a better carrier. |

### Findings

Each run also gets a list of plain-language findings, such as:
- each reference's offset and its PASS/FAIL against the limit
- Δ against the baseline, and repeatability
- significant drift and settling times
- readings excluded while unlocked, and invalid readings with the reasons
- periodic wobble, and a temperature coefficient when it correlates clearly
- short-term stability limited by measurement noise, weak carrier, too few GPS satellites
- an off-air stimulus limiting accuracy

---

## The engineering report

**Engineering report (HTML)** writes `report.html` into the run folder and opens it. It's a single file with no external dependencies that prints cleanly. It contains:

1. **Findings**
2. **Summary by reference:** offset, Hz, U, Δ vs baseline, σ, repeatability, drift, ADEV at 1/10/100/1000 s, settling, limit and result
3. **Test setup:**
   - radio model, nickname, serial, firmware and IP, and which references are fitted
   - the radio's `freq_error_ppb`
   - stimulus and its uncertainty
   - slice, dial, tone, DAX channel and gate
   - sweep plan, times and operator notes
4. **Charts:**
   - reference error over time, with settling shaded
   - deviation from each reference's mean
   - ADEV/MDEV/HDEV, and TDEV
   - histogram of readings
   - spectrum
   - radio temperatures, and offset against temperature
   - SNR and level
   - GPS satellites
5. **Segments table**
6. **Stability tables:** τ, ADEV, ±1σ, MDEV, HDEV, TDEV
7. **Temperature table**
8. **Periodic components table**
9. **Event log**
10. **Method:** how it was measured, the formulas, the exclusion rules and the measurement floor

The app also writes `analysis.json` next to it, with every computed number in machine-readable form.

---

## Comparing runs

The **Compare** tab puts several runs side by side: different radios, serial numbers, firmware, days or temperatures. It shows one row per run and reference, an overlaid ADEV chart, and each reference's offset ± U. **Export comparison report** saves it as an HTML file under `Documents\Flex Reference Lab\Comparisons\`.

---

## Files and data format

Runs live in **`Documents\Flex Reference Lab\Runs\<UTC date-time> <run name>\`**. If Windows *Controlled folder access* blocks Documents, they go to `%USERPROFILE%\Flex Reference Lab\Runs\` instead. Settings and logs are in `%APPDATA%\FlexRefLab\`.

| File | Contents |
|---|---|
| `samples.csv` | One row per gate, appended and flushed as it is measured, so a crash loses at most one reading |
| `run.json` | The run's description: radio, stimulus, slice, dial, tone, gate, plan, limits, temperature columns, `freq_error_ppb`, and the full event log |
| `report.html` | The engineering report (written when you ask for it) |
| `analysis.json` | All analysis results (written with the report) |

### `samples.csv` columns

| Column | Meaning |
|---|---|
| `time_utc` | End of the gate, UTC, millisecond resolution |
| `elapsed_s` | Seconds since the run started |
| `step` | Sweep step (0 for a single run) |
| `ref` | Reference in use: `TCXO`, `EXT10`, `GPSDO`, `GNSS`, `UNKNOWN` |
| `osc_setting`, `osc_state` | The radio's oscillator setting (auto/tcxo/gpsdo/external) and the oscillator actually in use |
| `locked` | 1 if the reference reported lock |
| `phase` | `measure` or `settle` |
| `valid`, `why` | Whether the reading is valid, and why not |
| `audio_hz` | Average tone frequency over the gate (counter-style, no dead time) |
| `fit_hz` | Least-squares tone frequency over the gate |
| `slice_err_mhz` | Slice tuning error from `slice get_error`, mHz |
| `apparent_hz` | Where the radio thinks the carrier is: dial + slice error + `audio_hz` |
| `err_hz` | `apparent_hz` − true carrier |
| `ppb`, `fit_ppb` | Reference error from `audio_hz` and from `fit_hz` (+ = reference fast) |
| `snr_db` | Carrier-to-noise ratio in the 100 Hz tracking bandwidth |
| `level_dbfs` | Carrier level |
| `lost_pkts` | DAX packets lost during the gate |
| `gps_tracked`, `gps_visible`, `gps_fix`, `gps_status` | The radio's GPS receiver status |
| `gps_freq_error` | GPS `freq_error`, exactly as the radio reports it (a fraction) |
| `temp_<name>` | One column per radio temperature sensor, °C (e.g. `temp_PATEMP`) |

---

## Choosing a stimulus

The result can never be better than the carrier you measure against.

| Stimulus | Typical uncertainty | Good for |
|---|---|---|
| **Signal generator locked to a GPSDO or rubidium standard** | ≪ 0.1 ppb | Everything, including GPSDO and GNSS at the sub-ppb level. **Use this for engineering results.** |
| **WWV / WWVH** (2.5, 5, 10, 15, 20, 25 MHz), off air | ±5–20 ppb, from ionospheric Doppler | TCXO offset; Δ between references (the Doppler is common to both and mostly cancels) |

- Set the carrier's **uncertainty** in Setup; it goes into U. The presets fill it in.
- Aim for an SNR above 20 dB, without overloading the radio.
- At 10 MHz, 1 ppb moves the tone by 10 mHz. The measurement floor at 30 dB SNR is a few tenths of a ppb at τ = 1 s, and it falls with averaging.

---

## Other sources: simulator and WSJT-X import

- **Simulator** (Setup → *Use the built-in simulator*): a pretend radio with a TCXO (180 ppb off, with a temperature coefficient and random walk), a GPSDO (with a 17 s, 0.12 ppb wobble) and an external input. It has lock delays, settling transients and the occasional lost packet. Use it for training, demos, and checking the app.
- **Import fmt.all** (Runs tab): turns a WSJT-X FreqCal measurement file into a run. You enter the dial frequency, the true carrier and the reference that was in use. Imported runs have no radio telemetry, and WSJT-X's own frequency calibration must have been zero.

---

## Command-line tools

Run these from the source folder with Go installed.

| Command | What it does |
|---|---|
| `go run ./cmd/probe -radio <nickname\|IP>` | Prints the radio's identity, reference, GPS, meters, slices and `slice get_error`. Changes nothing. |
| `go run ./cmd/probe -radio <…> -secs 60 -carrier 10e6` | Also tunes the slice, measures from DAX for 60 s, prints every reading, and restores the slice. Never switches references. |
| `go run ./cmd/oscset -radio <IP> [-set auto\|tcxo\|gpsdo\|external]` | Shows the oscillator state, or selects one |
| `go run ./cmd/reanalyze "<run folder>"…` | Rebuilds `report.html` and `analysis.json` from saved data, after the analysis improves |

---

## Limitations and known radio behaviour

- **A GUI station is needed** for a slice. If the radio has no GUI client connected, there's nothing to measure with.
- **The DAX channel is borrowed.** Don't use the same channel in SmartSDR DAX during a run.
- **Off-air stimuli** limit absolute accuracy, as described above. Use a lab carrier to verify GPSDO or GNSS to the ppb level.
- **Oscillator setting echo (FLEX-6600M, firmware 4.4.0):** when only the *setting* changes (e.g. gpsdo → auto, with the GPSDO still in use), the radio doesn't send status to the client that made the change. Even `sub radio all` keeps returning the old setting on that connection, while a new connection sees the new one. The app therefore records the setting itself once the radio replies OK.
- **GNSS labelling:** on a GNSS radio, the app doesn't yet know whether the radio reports `state=gpsdo` or `state=gnss`. It labels the reference GNSS if the radio reports a GNSS receiver and no GPSDO, and Setup can force the label either way.
- **The default PASS/FAIL limits are placeholders** (TCXO ±500, external ±10, GPSDO ±1, GNSS ±1 ppb). Set them to the real specification.

---

## Hardware tested

**FLEX-6600M**, firmware 4.4.0.42067, with a GPSDO and AetherSDR as the GUI station, on 28 October 2026, receiving WWV at 10 MHz. Verified:

- discovery, connecting and binding to the station
- reference status and GPS status
- meters over VITA-49: supply voltages, fan, `PATEMP`
- `slice get_error` (e.g. `+3.814698,-9.536743` mHz, which changes after a retune)
- DAX RX at 24 kHz with 0 lost packets
- carrier acquisition, a single run, and restoring the slice
- a **TCXO → GPSDO sweep**: both switches locked within 1 s and the setting was restored to `auto`

Measured: the **TCXO was −1450.8 ppb** relative to the GPSDO (Δ −1456.7 ± 0.9 ppb).

Not yet tried: the external 10 MHz input, and a GNSS-equipped radio.

---

## Building and testing

You need Go 1.26+ and the [Wails v2 CLI](https://wails.io).

```
wails build                      # → build\bin\FlexRefLab.exe
go test -short ./...             # unit tests
go test ./...                    # plus a ~2 min real-time simulator sweep
```

**Live tests** run against a real radio and are opt-in. Have a GUI client on slice A.

- **Single run:**

  ```
  set FLEXREFLAB_LIVE_RADIO=192.168.1.50
  go test -run TestLiveSingleRun -v .
  ```

- **Sweep:** set these as well, then run the same command.

  ```
  set FLEXREFLAB_LIVE_SWEEP=tcxo,gpsdo
  set FLEXREFLAB_LIVE_SETTLE=60
  set FLEXREFLAB_LIVE_DWELL=180
  ```

For development:
- `FLEXREFLAB_HOME=<folder>` keeps settings, logs and runs apart from a real install.
- `FLEXREFLAB_NO_LOCK=1` allows a second instance.

### Layout

| Path | Contents |
|---|---|
| `main.go`, `app.go` | Wails app and the UI bindings |
| `engine.go` | Run control, the sweep state machine, telemetry for each reading |
| `sim.go` | The simulated radio |
| `internal/flex` | SmartSDR API client: discovery, status, slices, DAX, meters, `get_error`, oscillator |
| `internal/measure` | Tone acquisition and phase tracking |
| `internal/analysis` | Statistics, the Allan family, segments, summaries, findings |
| `internal/plot`, `internal/report` | SVG charts and the HTML reports |
| `internal/store` | Run folders, `samples.csv`, `run.json` |
| `frontend/dist/index.html` | The UI (plain HTML/JS, no build step) |
| `cmd/` | `probe`, `oscset`, `reanalyze` |

---

## License

MIT © 2026 Michael VA3MW
