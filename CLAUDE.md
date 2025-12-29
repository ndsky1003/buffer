# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

This is a high-performance buffer pool library for Go v2 that provides automatic sizing and memory management for `bytes.Buffer` and `[]byte` objects. The library uses an adaptive pool with EMA (Exponential Moving Average) calibration to dynamically adjust buffer sizes based on usage patterns.

**IMPORTANT**: The README notes this library uses `sync.Pool` which depends on GC, potentially creating adverse cycles under certain conditions.

## Project Structure

- `buffer.go` - Core pool implementation with EMA calibration algorithm
- `api.go` - Convenience wrappers for `bytes.Buffer` and `[]byte` pools
- `option.go` - Configuration options using builder pattern
- `pool.go` - Experimental adaptive pool with per-CPU sharding (has compilation errors)
- `simulation_test.go` - Traffic evolution and cooldown tests

## Common Commands

### Build
```bash
go build ./...
```

### Run Tests
```bash
go test -v
```

### Run Specific Test
```bash
go test -v -run TestEvolution
go test -v -run TestCooldownDebug
```

## Architecture

### Core Algorithm (buffer.go)

The `Pool[T]` implements an auto-scaling buffer pool with three key components:

1. **Adapter Functions**: Generic type system using function pointers to abstract differences between `*bytes.Buffer` and `[]byte`
   - `makeFunc`: Creates new buffer with given capacity
   - `resetFunc`: Resets buffer for reuse
   - `statFunc`: Returns (used, capacity) stats

2. **EMA Calibration** (lines 166-212):
   - Asymmetric factors for fast growth/slow shrinkage
     - `emaUpFactor = 0.4`: Quick response to traffic increases
     - `emaDownFactor = 0.8`: Resists shrinking during temporary drops
   - `premiumFactor = 1.05`: Adds 5% buffer to prevent endless逼近
   - Calibrates every `calibratePeriod` Put operations (default: 1000)

3. **Smart Discard Strategy** (line 156):
   - Buffers exceeding `maxPercent * calibratedSize` are discarded instead of pooled
   - Prevents memory bloat from oversized buffers

4. **Cache Padding** (line 29):
   - Uses 64-byte padding to prevent false sharing between atomic variables
   - Critical for performance on multi-core systems

### Configuration (option.go)

Builder pattern for pool configuration:
- `SetMinSize(512)`: Minimum buffer size (default 512B)
- `SetMaxSize(64 << 20)`: Maximum buffer size (default 64MB)
- `SetMaxPercent(1.5)`: Discard threshold multiplier
- `SetCalibratePeriod(1000)`: Calibration frequency
- `SetCalibratedSz(1024)`: Initial calibrated size guess

### Known Issues

**pool.go** has a compilation error at line 118: undefined variable `now`. This file appears to be experimental/unused and may need fixing before use.

## Testing Notes

The simulation tests demonstrate three phases:
1. Stable low traffic (2KB)
2. Surge traffic (10KB) - observes rapid expansion
3. Cooldown (2KB) - observes slow contraction

**Critical**: Tests must actually write data to buffers (not just Grow), as the pool tracks `Len()` not `Cap()` for usage statistics. See `simulation_test.go:51-55`.
