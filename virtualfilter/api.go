package virtualfilter

import (
	"sync"
	"time"

	"github.com/Conflux-Chain/confura/node"
	"github.com/Conflux-Chain/confura/util"
	"github.com/Conflux-Chain/confura/util/metrics"
	rpcutil "github.com/Conflux-Chain/confura/util/rpc"
	"github.com/openweb3/go-rpc-provider"
	"github.com/openweb3/web3go/types"
	"github.com/sirupsen/logrus"
)

var (
	ethEmptyLogs = []types.Log{}
	nilRpcId     = rpc.ID("0x0")
)

// FilterApi offers support to proxy through full nodes to create and manage filters.
type FilterApi struct {
	sys       *FilterSystem
	filtersMu sync.Mutex
	filters   map[rpc.ID]*Filter
	clients   util.ConcurrentMap
}

// NewFilterApi returns a new FilterApi instance.
func NewFilterApi(system *FilterSystem, ttl time.Duration) *FilterApi {
	api := &FilterApi{
		sys:     system,
		filters: make(map[rpc.ID]*Filter),
	}

	go api.timeoutLoop(ttl)

	return api
}

// timeoutLoop runs at the interval set by 'timeout' and deletes filters
// that have not been recently used. It is started when the API is created.
func (api *FilterApi) timeoutLoop(timeout time.Duration) {
	ticker := time.NewTicker(timeout / 2)
	defer ticker.Stop()

	for range ticker.C {
		api.expireFilters(timeout)
	}
}

// NewBlockFilter creates a proxy block filter from full node with specified node URL
func (api *FilterApi) NewBlockFilter(nodeUrl string) (rpc.ID, error) {
	client, err := api.loadOrGetFnClient(nodeUrl)
	if err != nil {
		return nilRpcId, filterProxyError(err)
	}

	// create a proxy block filter to the allocated full node
	fid, err := client.Filter.NewBlockFilter()
	if err != nil {
		return nilRpcId, filterProxyError(err)
	}

	pfid := rpc.NewID()
	api.addFilter(pfid, newFilter(FilterTypeBlock, fnDelegateInfo{
		fid: *fid, nodeUrl: nodeUrl,
	}))

	return pfid, nil
}

// NewPendingTransactionFilter creates a proxy pending txn filter from full node with specified node URL
func (api *FilterApi) NewPendingTransactionFilter(nodeUrl string) (rpc.ID, error) {
	client, err := api.loadOrGetFnClient(nodeUrl)
	if err != nil {
		return nilRpcId, filterProxyError(err)
	}

	// create a proxy pending txn filter to the allocated full node
	fid, err := client.Filter.NewPendingTransactionFilter()
	if err != nil {
		return nilRpcId, filterProxyError(err)
	}

	pfid := rpc.NewID()
	api.addFilter(pfid, newFilter(FilterTypePendingTxn, fnDelegateInfo{
		fid: *fid, nodeUrl: nodeUrl,
	}))

	return pfid, nil
}

// NewFilter creates a proxy log filter from full node with specified node URL and filter query condition
func (api *FilterApi) NewFilter(nodeUrl string, crit types.FilterQuery) (rpc.ID, error) {
	w3c, err := api.loadOrGetFnClient(nodeUrl)
	if err != nil {
		return nilRpcId, filterProxyError(err)
	}

	metrics.UpdateEthLogFilter("eth_newFilter", w3c.Eth, &crit)

	var fid *rpc.ID
	if api.sys != nil { // create a delegate log filter to the allocated full node
		fid, err = api.sys.NewFilter(w3c, &crit)
	} else { // fallback to create a proxy log filter to full node
		fid, err = w3c.Filter.NewLogFilter(&crit)
	}

	if err != nil {
		return nilRpcId, filterProxyError(err)
	}

	pfid := rpc.NewID()
	api.addFilter(pfid, newFilter(FilterTypePendingTxn, fnDelegateInfo{
		fid: *fid, nodeUrl: nodeUrl,
	}, &crit))

	return pfid, nil
}

// UninstallFilter removes the proxy filter with the given filter id.
func (api *FilterApi) UninstallFilter(nodeUrl string, id rpc.ID) (bool, error) {
	f, found := api.delFilter(id)
	if !found {
		return false, nil
	}

	// Not the old delegate full node? This maybe due to client IP change or
	// rebalance for consistent hash ring of node manager
	if !f.IsDelegateFullNode(nodeUrl) {
		logrus.WithFields(logrus.Fields{
			"filterID": id,
			"newFnUrl": nodeUrl,
			"oldFnUrl": f.del.nodeUrl,
		}).Info("Delegate full node switched over for `UninstallFilter`")

		// but still use the old delegate full node for data consistency
		nodeUrl = f.del.nodeUrl
	}

	client, err := api.loadOrGetFnClient(nodeUrl)
	if err != nil {
		return false, filterProxyError(err)
	}

	if api.sys != nil && f.typ == FilterTypeLog {
		return api.sys.UninstallFilter(f.del.fid)
	}

	return client.Filter.UninstallFilter(f.del.fid)
}

// GetFilterLogs returns the logs for the proxy filter with the given id.
func (api *FilterApi) GetFilterLogs(nodeUrl string, id rpc.ID) (logs []types.Log, err error) {
	f, found := api.getFilter(id)

	if !found || f.typ != FilterTypeLog { // not found or wrong filter type?
		return nil, errFilterNotFound
	}

	// Not the old delegate full node? This maybe due to client IP change or
	// rebalance for consistent hash ring of node manager
	if !f.IsDelegateFullNode(nodeUrl) {
		logrus.WithFields(logrus.Fields{
			"filterID": id,
			"newFnUrl": nodeUrl,
			"oldFnUrl": f.del.nodeUrl,
		}).Info("Delegate full node switched over for `GetFilterLogs`")

		// but still use the old delegate full node for data consistency
		nodeUrl = f.del.nodeUrl
	}

	w3c, err := api.loadOrGetFnClient(nodeUrl)
	if err != nil {
		return nil, filterProxyError(err)
	}

	if api.sys != nil { // get filter logs from `FilterSystem`
		logs, err = api.sys.GetFilterLogs(f.del.fid)
	} else { // fallback to the full node for filter logs
		logs, err = w3c.Filter.GetFilterLogs(f.del.fid)
	}

	if IsFilterNotFoundError(err) {
		// remove deprecated proxy filter
		api.delFilter(id)
	}

	if logs == nil { // uniform empty logs
		logs = ethEmptyLogs
	}

	return logs, err
}

// GetFilterChanges returns the data for the proxy filter with the given id since
// last time it was called. This can be used for polling.
func (api *FilterApi) GetFilterChanges(nodeUrl string, id rpc.ID) (res *types.FilterChanges, err error) {
	f, found := api.getFilter(id)
	if !found {
		return nil, errFilterNotFound
	}

	// Not the old delegate full node? This maybe due to client IP change or
	// rebalance for consistent hash ring of node manager
	if !f.IsDelegateFullNode(nodeUrl) {
		logrus.WithFields(logrus.Fields{
			"filterID": id,
			"newFnUrl": nodeUrl,
			"oldFnUrl": f.del.nodeUrl,
		}).Info("Delegate full node switched over for `GetFilterChanges`")

		// but still use the old delegate full node to keep data consistency
		nodeUrl = f.del.nodeUrl
	}

	// refresh filter last polling time
	api.refreshFilterPollingTime(id)

	w3c, err := api.loadOrGetFnClient(nodeUrl)
	if err != nil {
		return nil, filterProxyError(err)
	}

	if api.sys != nil && f.typ == FilterTypeLog {
		// get filter changed logs from `FilterSystem`
		res, err = api.sys.GetFilterChanges(f.del.fid)
	} else {
		// otherwise fallback to the full node for filter changed logs,
		// or get filter changes from full node
		res, err = w3c.Filter.GetFilterChanges(f.del.fid)
	}

	if IsFilterNotFoundError(err) {
		// remove deprecated proxy filter
		api.delFilter(id)
	}

	return res, err
}

func (api *FilterApi) loadOrGetFnClient(nodeUrl string) (*node.Web3goClient, error) {
	nodeName := rpcutil.Url2NodeName(nodeUrl)
	client, _, err := api.clients.LoadOrStoreFnErr(nodeName, func(interface{}) (interface{}, error) {
		client, err := rpcutil.NewEthClient(nodeUrl, rpcutil.WithClientHookMetrics(true))
		if err != nil {
			return nil, err
		}

		return &node.Web3goClient{Client: client, URL: nodeUrl}, nil
	})

	if err != nil {
		return nil, err
	}

	return client.(*node.Web3goClient), nil
}

// proxy filter management

func (api *FilterApi) getFilter(id rpc.ID) (*Filter, bool) {
	api.filtersMu.Lock()
	defer api.filtersMu.Unlock()

	f, ok := api.filters[id]
	return f, ok
}

func (api *FilterApi) addFilter(id rpc.ID, filter *Filter) {
	api.filtersMu.Lock()
	defer api.filtersMu.Unlock()

	api.filters[id] = filter
}

func (api *FilterApi) delFilter(id rpc.ID) (*Filter, bool) {
	api.filtersMu.Lock()
	defer api.filtersMu.Unlock()

	f, found := api.filters[id]
	if found {
		delete(api.filters, id)
	}

	return f, found
}

func (api *FilterApi) refreshFilterPollingTime(id rpc.ID) {
	api.filtersMu.Lock()
	defer api.filtersMu.Unlock()

	if f, found := api.filters[id]; found {
		f.lastPollingTime = time.Now()
	}
}

func (api *FilterApi) expireFilters(ttl time.Duration) {
	api.filtersMu.Lock()
	defer api.filtersMu.Unlock()

	for id, f := range api.filters {
		if time.Since(f.lastPollingTime) < ttl { // not expired yet
			continue
		}

		delete(api.filters, id)

		if f.typ == FilterTypeLog {
			// also uninstall delegate log filters from `FilterSystem`
			api.sys.UninstallFilter(f.del.fid)
		}
	}
}