package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/PuerkitoBio/goquery"
	"github.com/clbanning/mxj"
	"github.com/lib/pq"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	AppId          string   `yaml:"app_id"`
	IosAppId       string   `yaml:"ios_app_id"`
	ReviewCount    int      `yaml:"review_count"`
	BotName        string   `yaml:"bot_name"`
	IconEmoji      string   `yaml:"icon_emoji"`
	MessageText    string   `yaml:"message_text"`
	IosMessageText string   `yaml:"ios_message_text"`
	WebHookUri     string   `yaml:"web_hook_uri"`
	Location       string   `yaml:location`
	IosLocations   []string `yaml:ioslocations`
}

type Review struct {
	Id        int
	Platform  string
	Author    string
	AuthorUri string `meddler:"author_uri"`
	Title     string
	Message   string
	Rate      string
	UpdatedAt time.Time `meddler:"updated_at,localtime"`
	Permalink string
	Color     string
}

type Reviews []Review

type DBH struct {
	*sql.DB
}

type SlackPayload struct {
	Text        string            `json:"text"`
	UserName    string            `json:"username"`
	IconEmoji   string            `json:"icon_emoji"`
	Attachments []SlackAttachment `json:"attachments"`
}

type SlackAttachment struct {
	Author     string                 `json: author_name`
	AuthorLink string                 `json: author_link`
	Title      string                 `json:"title"`
	TitleLink  string                 `json:"title_link"`
	Text       string                 `json:"text"`
	Fallback   string                 `json:"fallback"`
	Color      string                 `json: color`
	Fields     []SlackAttachmentField `json:"fields"`
}

type SlackAttachmentField struct {
	Title string `json:"title"`
	Value string `json:"value"`
	Short bool   `json:"short"`
}

type Map map[string]interface{}

const (
	TABLE_NAME                  = "review"
	BASE_URI                    = "https://play.google.com"
	IOS_BASE_URI                = "https://itunes.apple.com"
	REVIEW_CLASS_NAME           = ".single-review"
	AUTHOR_NAME_CLASS_NAME      = ".review-info span.author-name"
	REVIEW_DATE_CLASS_NAME      = ".review-info .review-date"
	REVIEW_TITLE_CLASS_NAME     = ".review-body .review-title"
	REVIEW_MESSAGE_CLASS_NAME   = ".review-body"
	REVIEW_LINK_CLASS_NAME      = ".review-link"
	REVIEW_RATE_CLASS_NAME      = ".review-info-star-rating .current-rating"
	RATING_EMOJI                = ":star:"
	RATING_EMOJI_2              = ":star2:"
	MAX_REVIEW_NUM              = 40
	REVIEW_PERMALINK_CLASS_NAME = ".review-info .reviews-permalink"
)

var (
	dbh        *DBH
	configFile = flag.String("c", "./config.yml", "config file")
)

func GetDBH() *DBH {
	return dbh
}

func (dbh *DBH) LastInsertId(tableName string) int {
	row := dbh.QueryRow(`SELECT id FROM ` + tableName + ` ORDER BY id DESC LIMIT 1`)

	var id int
	err := row.Scan(&id)
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			return 0
		}
		log.Fatal(err)
	}

	return id
}

func NewConfig(path string) (config Config, err error) {
	config = Config{}

	data, err := ioutil.ReadFile(path)
	if err != nil {
		return config, err
	}

	if err := yaml.Unmarshal(data, &config); err != nil {
		return config, err
	}

	if config.ReviewCount > MAX_REVIEW_NUM || config.ReviewCount < 1 {
		return config, fmt.Errorf("Please Set Num Between 1 and 40.")
	}

	url := os.Getenv("DATABASE_URL")
	connection, _ := pq.ParseURL(url)
	if "" == os.Getenv("DISABLE_DATABASE_SSL") {
		connection += " sslmode=require"
	}
	db, err := sql.Open("postgres", connection)
	if err != nil {
		return config, err
	}

	err = db.Ping()
	if err != nil {
		return config, err
	}

	dbh = &DBH{db}

	// override BotName if environment variable found
	botName := os.Getenv("JON_SNOW_BOT_NAME")
	if botName != "" {
		config.BotName = botName
	}

	// override AppId if environment variable found
	appId := os.Getenv("JON_SNOW_APP_ID")
	if appId != "" {
		config.AppId = appId
	}

	// override IosAppId if environment variable found
	iosAppId := os.Getenv("JON_SNOW_IOS_APP_ID")
	if iosAppId != "" {
		config.IosAppId = iosAppId
	}

	// override WebHookUri if environment variable found
	webHookUri := os.Getenv("JON_SNOW_SLACK_HOOK")
	if webHookUri != "" {
		config.WebHookUri = webHookUri
	}

	// override Location if environment variable found
	location := os.Getenv("JON_SNOW_LOCATION")
	if location != "" {
		config.Location = location
	}

	if config.AppId == "" {
		return config, fmt.Errorf("Please Set Your Google Play App Id.")
	}

	uri := fmt.Sprintf("%s/store/apps/details?id=%s", BASE_URI, config.AppId)

	res, err := http.Get(uri)
	if err != nil {
		return config, err
	}

	if res.StatusCode == http.StatusNotFound {
		return config, fmt.Errorf("AppID: %s is not exists", config.AppId)
	}

	// iOS App is optional
	if config.IosAppId != "" {
		// "foo-bar" can be any thing, apple get app info by id
		uri = fmt.Sprintf("%s/app/foo-bar/id%s", IOS_BASE_URI, config.IosAppId)

		res, err = http.Get(uri)
		if err != nil {
			return config, err
		}

		if res.StatusCode == http.StatusNotFound {
			return config, fmt.Errorf("IosAppID: %s is not exists", config.IosAppId)
		}
	}

	return config, err
}

func main() {
	flag.Parse()

	config, err := NewConfig(*configFile)
	if err != nil {
		printError(err)
		return
	}
	log.Println(config)
	err = ProcessGooglePlayReview(config)
	if err != nil {
		printError(err)
		return
	}

	err = ProcessIosReview(config)
	if err != nil {
		printError(err)
		return
	}

	log.Println("done")
}

func ProcessGooglePlayReview(config Config) error {

	reviews, err := GetReview(config)
	if err != nil {
		return err
	}
	reviews, err = SaveReviews(reviews)
	if err != nil {
		return err
	}

	err = PostReview(config, reviews)
	if err != nil {
		return err
	}

	return nil
}

func ProcessIosReview(config Config) error {
	if config.IosAppId != "" {
		reviews, err := GetIosReview(config)
		if err != nil {
			return err
		}

		reviews, err = SaveReviews(reviews)
		if err != nil {
			return err
		}

		err = PostReview(config, reviews)
		if err != nil {
			return err
		}
	}
	return nil
}

func GetIosReview(config Config) (Reviews, error) {
	reviews := Reviews{}

	for _, location := range config.IosLocations {
		uri := fmt.Sprintf("%s/%s/rss/customerreviews/page=1/id=%s/sortBy=mostRecent/xml", IOS_BASE_URI, location, config.IosAppId)
		log.Println(uri)
		response, err := http.Get(uri)
		if err != nil {
			printError(err)
			continue
		} else {
			defer response.Body.Close()
			contents, err := ioutil.ReadAll(response.Body)

			if err != nil {
				printError(err)
				continue
			}
			data, err := mxj.NewMapXml(contents)
			if err != nil {
				printError(err)
				log.Fatal("cant parse xml")
			}

			entries, err := data.ValuesForPath("feed.entry")
			if err != nil {
				printError(err)
				log.Fatal("cant get entries")
			}
			for i, entry := range entries {
				if i == 0 {
					continue
				}
				rate, _ := strconv.Atoi(entry.(map[string]interface{})["rating"].(string))
				commonData := entry.(map[string]interface{})
				author := commonData["author"].(map[string]interface{})

				updatedAt, _ := time.Parse(time.RFC3339, commonData["updated"].(string))
				if err != nil {
					printError(err)
					log.Fatal("cant get time")
				}

				message := commonData["content"].([]interface{})[0].(map[string]interface{})["#text"].(string)

				review := Review{
					Author:    author["name"].(string),
					Platform:  "ios",
					AuthorUri: author["uri"].(string),
					Title:     commonData["title"].(string),
					Rate:      strings.Repeat(RATING_EMOJI, rate),
					Message:   message,
					UpdatedAt: updatedAt,
				}

				reviews = append(reviews, review)
			}
		}
	}
	sort.Sort(reviews)
	return reviews, nil
}
func GetReview(config Config) (Reviews, error) {
	reviews := Reviews{}
	var r http.Request
	r.ParseForm()
	r.Form.Add("reviewType", "0")
	r.Form.Add("pageNum", "0")
	r.Form.Add("id", config.AppId)
	r.Form.Add("reviewSortOrder", "0")
	r.Form.Add("xhr", "1")
	r.Form.Add("hl", config.Location)
	bodystr := strings.TrimSpace(r.Form.Encode())
	request, err := http.NewRequest("POST", BASE_URI+"/store/getreviews?authuser=0", strings.NewReader(bodystr))
	if err != nil {
		printError(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	var resp *http.Response
	resp, err = http.DefaultClient.Do(request)
	if err != nil {
		printError(err)
	}
	result, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		printError(err)
	}
	data := result[7 : len(result)-1]
	var html string
	var wo []interface{}
	err = json.Unmarshal(data, &wo)
	html = wo[2].(string)
	if err != nil {
		printError(err)
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		printError(err)
	}

	doc.Find(REVIEW_CLASS_NAME).Each(func(i int, s *goquery.Selection) {
		authorNode := s.Find(AUTHOR_NAME_CLASS_NAME)

		authorName := authorNode.Text()

		dateNode := s.Find(REVIEW_DATE_CLASS_NAME)

		var timeForm string
		if config.Location == "zh-tw" {
			timeForm = "2006年1月2日"
		} else {
			timeForm = "January 2, 2006"
		}

		date, err := time.Parse(timeForm, dateNode.Text())
		if err != nil {
			log.Println(err)
			return
		}

		reviewPermalinkNode := s.Find(REVIEW_PERMALINK_CLASS_NAME)
		reviewPermalink, _ := reviewPermalinkNode.Attr("href")

		reviewTitle := s.Find(REVIEW_TITLE_CLASS_NAME).Text()
		if len(reviewTitle) == 0 {
			reviewTitle = "No title provided"
		}

		reviewMessage := s.Find(REVIEW_MESSAGE_CLASS_NAME).Text()
		reviewLink := s.Find(REVIEW_LINK_CLASS_NAME).Text()

		reviewMessage = strings.Split(reviewMessage, reviewLink)[0]

		reviewRateNode := s.Find(REVIEW_RATE_CLASS_NAME)
		rateMessage, _ := reviewRateNode.Attr("style")

		rate := parseRate(rateMessage)

		review := Review{
			Author:    authorName,
			Platform:  "andirod",
			AuthorUri: reviewPermalink,
			Title:     reviewTitle,
			Message:   reviewMessage,
			Rate:      rate,
			UpdatedAt: date,
			Permalink: reviewPermalink,
		}
		reviews = append(reviews, review)
	})

	sort.Sort(reviews)

	return reviews, nil
}

func parseRate(message string) string {
	rateMessage := ""

	switch {
	case strings.Contains(message, "width: 20%"):
		rateMessage = strings.Repeat(RATING_EMOJI, 1)
	case strings.Contains(message, "width: 40%"):
		rateMessage = strings.Repeat(RATING_EMOJI, 2)
	case strings.Contains(message, "width: 60%"):
		rateMessage = strings.Repeat(RATING_EMOJI, 3)
	case strings.Contains(message, "width: 80%"):
		rateMessage = strings.Repeat(RATING_EMOJI, 4)
	case strings.Contains(message, "width: 100%"):
		rateMessage = strings.Repeat(RATING_EMOJI_2, 5)
	}

	return rateMessage
}

func SaveReviews(reviews Reviews) (Reviews, error) {
	postReviews := Reviews{}

	for _, review := range reviews {
		var id int
		row := dbh.QueryRow("SELECT id FROM review WHERE author_uri = $1", review.AuthorUri)
		err := row.Scan(&id)

		if err != nil {
			if err.Error() != "sql: no rows in result set" {
				return postReviews, err
			}
		}

		if id == 0 {
			_, err := dbh.Exec("INSERT INTO review (platform, author, author_uri, updated_at) VALUES ($1, $2, $3, $4)",
				review.Platform, review.Author, review.AuthorUri, review.UpdatedAt)
			if err != nil {
				return postReviews, err
			}
			postReviews = append(postReviews, review)
		}
	}

	return postReviews, nil
}

func PostReview(config Config, reviews Reviews) error {
	attachments := []SlackAttachment{}

	if 1 > len(reviews) {
		return nil
	}

	for i, review := range reviews {
		if i >= config.ReviewCount {
			break
		}

		fields := []SlackAttachmentField{}

		fields = append(fields, SlackAttachmentField{
			Title: "Rating",
			Value: review.Rate,
			Short: true,
		})

		fields = append(fields, SlackAttachmentField{
			Title: "UpdatedAt",
			Value: review.UpdatedAt.Format("2006-01-02"),
			Short: true,
		})

		var titleLink string
		if review.Platform == "ios" {
			titleLink = review.AuthorUri
		} else {
			titleLink = fmt.Sprintf("%s%s", BASE_URI, review.Permalink)
		}

		attachments = append(attachments, SlackAttachment{
			Title:     review.Author,
			TitleLink: titleLink,
			Text:      "*" + review.Title + "*\n" + review.Message,
			Fallback:  review.Message + " " + review.AuthorUri,
			Color:     review.Color,
			Fields:    fields,
		})
	}

	var messageText string
	if reviews[0].Platform == "ios" {
		messageText = config.IosMessageText
	} else {
		messageText = config.MessageText
	}
	slackPayload := SlackPayload{
		UserName:    config.BotName,
		IconEmoji:   config.IconEmoji,
		Text:        messageText,
		Attachments: attachments,
	}
	payload, err := json.Marshal(slackPayload)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", config.WebHookUri, bytes.NewBuffer([]byte(payload)))
	req.Header.Set("Content-Type", "application/json")
	if err != nil {
		return err
	}
	client := http.DefaultClient
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	return nil
}

func (r Reviews) Len() int {
	return len(r)
}

func (r Reviews) Swap(i, j int) {
	r[i], r[j] = r[j], r[i]
}

func (r Reviews) Less(i, j int) bool {
	return r[i].UpdatedAt.Unix() > r[j].UpdatedAt.Unix()
}

func printError(err error) {
	var stack [4096]byte
	runtime.Stack(stack[:], false)
	log.Printf("%q\n%s\n", err, stack[:])

}
