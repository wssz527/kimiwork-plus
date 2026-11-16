package bridge

import "os"

func (n *Native) reconcile(previous os.FileInfo) os.FileInfo {
	n.service.mutation.Lock()
	defer n.service.mutation.Unlock()
	n.profileMu.Lock()
	defer n.profileMu.Unlock()
	path := n.policies.subagents.ConfigPath()
	current, err := os.Stat(path)
	if err != nil || !current.Mode().IsRegular() {
		return nil
	}
	if previous != nil && os.SameFile(previous, current) && previous.Size() == current.Size() && previous.ModTime() == current.ModTime() {
		return previous
	}
	d, _, err := n.service.Store.Read()
	if err != nil || n.policies.Configure(d) != nil {
		return previous
	}
	n.policies.mu.Lock()
	n.policies.services.Update(n.policies.services.Intent())
	n.policies.mu.Unlock()
	observed, _ := os.Stat(path)
	return observed
}
