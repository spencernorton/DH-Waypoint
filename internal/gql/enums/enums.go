package enums

import (
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/model"
)

type enum struct {
	Name   string
	Values []string
}

func newEnum(name string, values []string) enum {
	return enum{
		Name:   name,
		Values: values,
	}
}

var Enums = []enum{
	newEnum("ContentType", model.ContentTypeNames()),
	newEnum("FacetLogic", model.FacetLogicNames()),
	newEnum("FileType", model.FileTypeNames()),
	newEnum("FilesStatus", model.FilesStatusNames()),
	newEnum("Language", model.LanguageValueStrings()),
	newEnum("EnglishAudio", model.EnglishAudioNames()),
	newEnum("Video3D", model.Video3DNames()),
	newEnum("VideoCodec", model.VideoCodecNames()),
	newEnum("VideoModifier", model.VideoModifierNames()),
	newEnum("VideoResolution", model.VideoResolutionNames()),
	newEnum("VideoSource", model.VideoSourceNames()),
	newEnum("TorrentContentOrderByField", search.TorrentContentOrderByNames()),
	newEnum("TorrentFilesOrderByField", search.TorrentFilesOrderByNames()),
	newEnum("QueueJobsOrderByField", search.QueueJobsOrderByNames()),
}
