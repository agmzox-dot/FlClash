package statistic

type RequestNotify func(c Tracker)
type RequestTrafficNotify func(c Tracker, uploadDelta, downloadDelta int64) bool
type RequestCloseNotify func(c Tracker)

var (
	DefaultRequestNotify        RequestNotify
	DefaultRequestTrafficNotify RequestTrafficNotify
	DefaultRequestCloseNotify   RequestCloseNotify
)

func (m *Manager) TotalTraffic(onlyProxy bool) (up, down int64) {
	if onlyProxy {
		return m.proxyUploadTotal.Load(), m.proxyDownloadTotal.Load()
	}
	return m.uploadTotal.Load(), m.downloadTotal.Load()
}

func (m *Manager) NowTraffic(onlyProxy bool) (up, down int64) {
	if onlyProxy {
		return m.proxyUploadBlip.Load(), m.proxyDownloadBlip.Load()
	}
	return m.uploadBlip.Load(), m.downloadBlip.Load()
}
