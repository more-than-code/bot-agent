package collect

import "testing"

func TestSelectKeepsServiceOutsideTop(t *testing.T) {
	cpus := make([]float64, 20)
	all := make([]ProcView, 0, 41)
	for i := 0; i < 20; i++ {
		cpus[i] = float64(100 - i)
		all = append(all, ProcView{PID: i + 1, CPU: &cpus[i], RSS: 10})
	}
	low := 0.1
	for i := 0; i < 20; i++ {
		all = append(all, ProcView{PID: 100 + i, CPU: &low, RSS: int64(100000 - i)})
	}
	tiny := 0.01
	all = append(all, ProcView{PID: 999, CPU: &tiny, RSS: 1, Service: "nginx", Name: "nginx"})
	got := Select(all)
	for _, p := range got {
		if p.PID == 999 {
			return
		}
	}
	t.Fatal("service process outside both top-20 lists was dropped")
}
