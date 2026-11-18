package songs

import (
	"context"
	"strings"
	"time"

	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/spotify"
	"github.com/miwidot/darkbot/internal/ytmusic"
)

// coverMinWidth: the overlay shows covers at most about this wide, so the
// smallest Spotify image at least this big is enough.
const coverMinWidth = 200

// Spotify queues on the device that is currently playing; Add to Queue and
// Skip need Spotify Premium (Web API reference).
type Spotify struct{ C *spotify.Client }

func (Spotify) Service() string   { return config.ServiceSpotify }
func (Spotify) Name() string      { return "Spotify" }
func (p Spotify) Connected() bool { return p.C.Current() != nil }

func (Spotify) ParseLink(text string) (string, bool, error) { return spotify.ParseLink(text) }

func (p Spotify) Track(ctx context.Context, id string) (*Track, error) {
	t, err := p.C.GetTrack(ctx, id)
	if err != nil {
		return nil, err
	}
	return fromSpotify(t), nil
}

func (p Spotify) Search(ctx context.Context, query string) (*Track, error) {
	t, err := p.C.SearchTrack(ctx, query)
	if err != nil {
		return nil, err
	}
	return fromSpotify(t), nil
}

func (p Spotify) Add(ctx context.Context, t Track) error { return p.C.AddToQueue(ctx, t.ID) }
func (p Spotify) Skip(ctx context.Context) error         { return p.C.Next(ctx) }

func (p Spotify) Playing(ctx context.Context) (bool, error) { return p.C.Playing(ctx) }

func (p Spotify) Now(ctx context.Context) (*Now, error) {
	pb, err := p.C.CurrentlyPlaying(ctx)
	if err != nil || pb == nil {
		return nil, err
	}
	names := make([]string, 0, len(pb.Item.Artists))
	for _, a := range pb.Item.Artists {
		names = append(names, a.Name)
	}
	return &Now{
		// The queue knows songs by URI, so requesters are found by it too.
		ID: pb.Item.URI, Title: pb.Item.Name, Artist: strings.Join(names, ", "),
		Cover: pb.Cover(coverMinWidth), Duration: pb.Item.Duration(),
		Progress: time.Duration(pb.ProgressMS) * time.Millisecond, Playing: pb.IsPlaying,
	}, nil
}

func (p Spotify) Queue(ctx context.Context) (*Queue, error) {
	q, err := p.C.GetQueue(ctx)
	if err != nil {
		return nil, err
	}
	out := &Queue{}
	if q.Current != nil {
		out.Current = fromSpotify(q.Current)
	}
	for i := range q.Next {
		out.Next = append(out.Next, *fromSpotify(&q.Next[i]))
	}
	return out, nil
}

// fromSpotify: Spotify queues by URI. is_playable is only set with track
// relinking; missing means playable.
func fromSpotify(t *spotify.Track) *Track {
	return &Track{
		ID: t.URI, Title: t.Title(), Duration: t.Duration(), Explicit: t.Explicit,
		Unplayable: t.IsPlayable != nil && !*t.IsPlayable,
	}
}

// YTMusic queues on YouTube Music in Pear Desktop. Enabled is the
// streamer's switch in the song settings.
type YTMusic struct {
	C       *ytmusic.Client
	Enabled func() bool
}

func (YTMusic) Service() string   { return config.ServiceYTMusic }
func (YTMusic) Name() string      { return "YouTube Music" }
func (p YTMusic) Connected() bool { return p.Enabled() && p.C.Linked() }

func (YTMusic) ParseLink(text string) (string, bool, error) { return ytmusic.ParseLink(text) }

func (p YTMusic) Track(ctx context.Context, id string) (*Track, error) {
	t, err := p.C.Lookup(ctx, id)
	if err != nil {
		return nil, err
	}
	return fromYTMusic(t), nil
}

func (p YTMusic) Search(ctx context.Context, query string) (*Track, error) {
	t, err := p.C.Search(ctx, query)
	if err != nil {
		return nil, err
	}
	return fromYTMusic(t), nil
}

func (p YTMusic) Add(ctx context.Context, t Track) error { return p.C.Add(ctx, t.ID) }
func (p YTMusic) Skip(ctx context.Context) error         { return p.C.Next(ctx) }

func (p YTMusic) Playing(ctx context.Context) (bool, error) { return p.C.Playing(ctx) }

func (p YTMusic) Now(ctx context.Context) (*Now, error) {
	s, err := p.C.Song(ctx)
	if err != nil || s == nil {
		return nil, err
	}
	return &Now{
		ID: s.VideoID, Title: s.Title, Artist: s.Artist, Cover: s.ImageSrc,
		Duration: seconds(s.SongDuration), Progress: seconds(s.ElapsedSeconds), Playing: s.Playing(),
	}, nil
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

func (p YTMusic) Queue(ctx context.Context) (*Queue, error) {
	q, err := p.C.Queue(ctx)
	if err != nil {
		return nil, err
	}
	out := &Queue{}
	if q.Current != nil {
		out.Current = fromYTMusic(q.Current)
	}
	for i := range q.Next {
		out.Next = append(out.Next, *fromYTMusic(&q.Next[i]))
	}
	return out, nil
}

func fromYTMusic(t *ytmusic.Track) *Track {
	return &Track{ID: t.VideoID, Title: t.Title(), Duration: t.Duration, Explicit: t.Explicit}
}
