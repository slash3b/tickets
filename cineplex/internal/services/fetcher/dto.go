//nolint:tagliatelle
package fetcher

type FilterRequest struct {
	ByFormat      string `json:"by_format"`
	ByQuality     string `json:"by_quality"`
	ByAudioFormat string `json:"by_audio_format"`
	ByCinema      string `json:"by_cinema"`
	ByStatus      string `json:"by_status"`
}

type CineplexMoviesResponse struct {
	Movies []CineplexMovie `json:"movies"`
}

// CineplexMovie api representation.
type CineplexMovie struct {
	IDMovie             string       `json:"id_movie"`
	IDCinema            string       `json:"id_cinema"`
	Title               string       `json:"title"`
	ReleaseDate         string       `json:"release_date"`
	Length              string       `json:"length"`
	OriginalTitle       string       `json:"original_title"`
	CinemaDate          string       `json:"cinema_date"`
	Language            string       `json:"language"`
	Captioning          string       `json:"captioning"`
	Format              string       `json:"format"`
	Rating              string       `json:"rating"`
	Directors           string       `json:"directors"`
	Actors              string       `json:"actors"`
	Country             string       `json:"country"`
	Producer            string       `json:"producer"`
	ProductionYear      string       `json:"production_year"`
	Poster              string       `json:"poster"`
	Trailer             string       `json:"trailer"`
	Genre               string       `json:"genre"`
	GenreEn             string       `json:"genre_en"`
	Site                any          `json:"site"`
	RankImdb            string       `json:"rank_imdb"`
	Synopsis            string       `json:"synopsis"`
	SynopsisEn          string       `json:"synopsis_en"`
	Link                string       `json:"link"`
	IsPromoted          string       `json:"is_promoted"`
	Type                string       `json:"type"`
	ComingSoon          string       `json:"coming_soon"`
	OrangeCode          string       `json:"orange_code"`
	PosterLarge         string       `json:"poster_large"`
	UUID                string       `json:"uuid"`
	ShowOnSlider        string       `json:"show_on_slider"`
	IDParent            any          `json:"id_parent"`
	SynopsisRu          string       `json:"synopsis_ru"`
	ProducerRu          string       `json:"producer_ru"`
	DirectorRu          string       `json:"director_ru"`
	ActorsRu            string       `json:"actors_ru"`
	GenreRu             string       `json:"genre_ru"`
	LanguageRu          string       `json:"language_ru"`
	LanguageEnglish     string       `json:"language_english"`
	LanguageText        string       `json:"language_text"`
	LanguageTextEnglish string       `json:"language_text_english"`
	LanguageTextRu      string       `json:"language_text_ru"`
	TitleRu             string       `json:"title_ru"`
	Sound               string       `json:"sound"`
	SoundEn             string       `json:"sound_en"`
	SoundRu             string       `json:"sound_ru"`
	Code                string       `json:"code"`
	ParentCode          any          `json:"parent_code"`
	PosterRu            string       `json:"poster_ru"`
	TrailerRu           string       `json:"trailer_ru"`
	PosterLargeRu       string       `json:"poster_large_ru"`
	Languages           string       `json:"languages"`
	Formats             string       `json:"formats"`
	Badge               string       `json:"badge"`
	Sorting             string       `json:"sorting"`
	TitleRo             string       `json:"title_ro"`
	TitleEn             string       `json:"title_en"`
	Events              []MovieEvent `json:"events"`
}

type MovieEvent struct {
	IDCinema string `json:"id_cinema"`
	IDMovie  string `json:"id_movie"`
	IDEvent  string `json:"id_event"`
	Date     string `json:"date"`
	IDRoom   string `json:"id_room"`
	Room     string `json:"room"`
}
