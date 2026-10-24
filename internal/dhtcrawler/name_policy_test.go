package dhtcrawler

import (
	"context"
	"fmt"
	mi "github.com/anacrolix/torrent/metainfo"
	"net/netip"
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/blocking"
	"github.com/spencernorton/DH-Waypoint/internal/csamblocklist"
	"github.com/spencernorton/DH-Waypoint/internal/namepolicy"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo/metainforequester"
	"github.com/stretchr/testify/require"
)

type policyCsam struct{ csamblocklist.Manager }

func (policyCsam) IsBlocked(protocol.InfoHash) bool { return false }

type policyRequester struct {
	calls int
	info  metainfo.Info
	ids   *protocol.TorrentIdentities
}

func (r *policyRequester) Request(_ context.Context, key protocol.ID, _ netip.AddrPort) (metainforequester.Response, error) {
	r.calls++
	h := protocol.InfoHashV1(key)
	ids := protocol.TorrentIdentities{V1: &h}
	if r.ids != nil {
		ids = *r.ids
	}
	return metainforequester.Response{Info: r.info, Validated: metainfo.ValidatedInfo{Info: r.info, Identities: ids}}, nil
}

type completeHashBlock struct {
	blocking.Manager
	hash protocol.InfoHash
	seen bool
}

func (b *completeHashBlock) Filter(_ context.Context, hs []protocol.InfoHash) ([]protocol.InfoHash, error) {
	var kept []protocol.InfoHash
	for _, h := range hs {
		if h == b.hash {
			b.seen = true
		} else {
			kept = append(kept, h)
		}
	}
	return kept, nil
}

func TestCrawlerRechecksFullV2BlockAfterDiscovery(t *testing.T) {
	v2 := protocol.InfoHashV2([32]byte{42})
	v1 := protocol.InfoHashV1(protocol.ID{24})
	for _, hybrid := range []bool{false, true} {
		t.Run(fmt.Sprint(hybrid), func(t *testing.T) {
			ids := protocol.TorrentIdentities{V2: &v2}
			key := v2.WireID()
			if hybrid {
				ids.V1 = &v1
				key = v1.WireID()
			}
			r := &policyRequester{info: metainfo.Info{Name: "Synthetic.Allowed.txt"}, ids: &ids}
			block := &completeHashBlock{hash: v2}
			banning := &policyBanning{}
			c := crawler{metainfoRequester: r, csamBlocklist: policyCsam{}, banningChecker: banning, blockingManager: block}
			_, err := c.doRequestMetaInfo(context.Background(), key, []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:1")})
			require.ErrorIs(t, err, namepolicy.ErrExcluded)
			require.True(t, block.seen)
			require.Zero(t, banning.calls)
		})
	}
}

type policyBanning struct{ calls int }

func (b *policyBanning) Check(metainfo.Info) error { b.calls++; return nil }

func TestNamePolicyStopsAfterMinimumMetadataBeforeBanningAndBlocks(t *testing.T) {
	p, err := namepolicy.New(namepolicy.Config{Enabled: true})
	require.NoError(t, err)
	for _, name := range []string{"Synthetic.电影.ENG.mkv", "Synthetic.Фильм.Dub.mkv", "FetishXXX.mkv"} {
		r := &policyRequester{info: metainfo.Info{Name: name}}
		b := &policyBanning{}
		c := crawler{namePolicy: p, metainfoRequester: r, banningChecker: b, csamBlocklist: policyCsam{}}
		_, err := c.doRequestMetaInfo(context.Background(), protocol.ID{1}, []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:1")})
		require.ErrorIs(t, err, namepolicy.ErrExcluded)
		require.Equal(t, 1, r.calls)
		require.Zero(t, b.calls)
		// blockingManager is deliberately nil: any destructive fallback panics.
	}
}
func TestOwnerHashDoesNotAcquireNameMetadata(t *testing.T) {
	h := protocol.ID{1}
	p, err := namepolicy.New(namepolicy.Config{Enabled: true, ExcludedInfoHashes: []string{h.String()}})
	require.NoError(t, err)
	c := crawler{namePolicy: p}
	_, err = c.doRequestMetaInfo(context.Background(), h, nil)
	require.ErrorIs(t, err, namepolicy.ErrExcluded)
}
func TestAllowedNameIgnoresForeignSupportPaths(t *testing.T) {
	p, err := namepolicy.New(namepolicy.Config{Enabled: true})
	require.NoError(t, err)
	r := &policyRequester{info: metainfo.Info{Name: "Allowed.Café.ENG.mkv", Files: []mi.FileInfo{{Path: []string{"电影", "Субтитры.srt"}, Length: 10}}}}
	b := &policyBanning{}
	c := crawler{namePolicy: p, metainfoRequester: r, banningChecker: b, csamBlocklist: policyCsam{}}
	_, err = c.doRequestMetaInfo(context.Background(), protocol.ID{1}, []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:1")})
	require.NoError(t, err)
	require.Equal(t, 1, b.calls)
}
