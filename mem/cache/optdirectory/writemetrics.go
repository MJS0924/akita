package optdirectory

// Exp-W: write workload metrics emitted once per kernel boundary.
// CSV: motivation_write_GPU{N}.csv
// Columns: sim_time, gpu_id, kernel_id,
//   write_event_count, sharer_set_churn_count, false_invalidation_count

import (
	"fmt"
	"os"

	"github.com/sarchlab/akita/v4/sim"
)

var (
	writeCSVFiles  = make(map[string]*os.File)
	writeCSVHeader = "sim_time,gpu_id,kernel_id,write_event_count,sharer_set_churn_count,false_invalidation_count\n"
)

func openWriteCSV(filename string) *os.File {
	csvMu.Lock()
	defer csvMu.Unlock()

	if f, ok := writeCSVFiles[filename]; ok {
		return f
	}
	f, err := os.Create(filename)
	if err != nil {
		panic(fmt.Sprintf("optdirectory: cannot create write CSV %s: %v", filename, err))
	}
	if _, err := f.WriteString(writeCSVHeader); err != nil {
		panic(fmt.Sprintf("optdirectory: cannot write write CSV header %s: %v", filename, err))
	}
	writeCSVFiles[filename] = f
	return f
}

// emitWriteMetrics writes one row to the write-metrics CSV for this kernel,
// then resets kernel-local write counters.
// Called only when at least one write occurred; skips zero-write kernels to
// keep the CSV lean (but still resets counters unconditionally).
func (c *Comp) emitWriteMetrics(simTime sim.VTimeInSec, kernelID int) {
	writes := c.writeEventCountKernel
	churn := c.sharerSetChurnCountKernel
	falseInv := c.falseInvalidCountKernel

	if writes > 0 {
		filename := fmt.Sprintf("motivation_write_GPU%d.csv", c.deviceID)
		f := openWriteCSV(filename)
		csvMu.Lock()
		fmt.Fprintf(f, "%.9f,%d,%d,%d,%d,%d\n",
			float64(simTime),
			c.deviceID,
			kernelID,
			writes,
			churn,
			falseInv,
		)
		csvMu.Unlock()
	}

	// Reset kernel-local write state unconditionally.
	c.writeEventCountKernel = 0
	c.sharerSetChurnCountKernel = 0
	c.falseInvalidCountKernel = 0
	c.prevSharerSetKernel = make(map[uint64]uint64)
	c.hmg4CLWritersKernel = make(map[uint64]uint64)
}