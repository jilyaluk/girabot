package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"time"

	"golang.org/x/oauth2"
	tele "gopkg.in/telebot.v3"
	"gopkg.in/telebot.v3/middleware"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/jilyaluk/girabot/internal/gira"
	"github.com/jilyaluk/girabot/internal/giraauth"
)

type User struct {
	// ID is a telegram user ID
	ID int64 `gorm:"primarykey"`

	CreatedAt time.Time

	TGName     string
	TGUsername string

	// State is a state of user
	State UserState

	Email          string
	EmailMessageID int

	Favorites         map[gira.StationSerial]string `gorm:"serializer:json"`
	EditingStationFav gira.StationSerial

	CurrentTripCode         gira.TripCode
	CurrentTripMessageID    string
	RateMessageID           string
	CurrentTripRating       gira.TripRating `gorm:"serializer:json"`
	CurrentTripRateAwaiting bool

	FinishedTrips int

	// either stations sorted by distance or favorites sorted by name
	LastSearchResults []gira.StationSerial `gorm:"serializer:json"`
	// if nil, will not show distances
	LastSearchLocation *tele.Location `gorm:"serializer:json"`

	SentDonateMessage bool
}

func (c *customContext) getActiveTripMsg() tele.Editable {
	return tele.StoredMessage{
		ChatID:    c.user.ID,
		MessageID: c.user.CurrentTripMessageID,
	}
}

func (c *customContext) getRateMsg() tele.Editable {
	return tele.StoredMessage{
		ChatID:    c.user.ID,
		MessageID: c.user.RateMessageID,
	}

}

// filteredUser is a User with some fields filtered out for logging.
type filteredUser User

func (u filteredUser) String() string {
	if u.Email != "" {
		u.Email = "<email>"
	}
	if u.LastSearchLocation != nil {
		u.LastSearchLocation = &tele.Location{Lat: 1, Lng: 1}
	}
	// print only number of results
	u.LastSearchResults = []gira.StationSerial{
		gira.StationSerial(fmt.Sprint(len(u.LastSearchResults))),
	}
	u.Favorites = map[gira.StationSerial]string{
		gira.StationSerial(fmt.Sprint(len(u.Favorites))): "",
	}
	return fmt.Sprintf("%+v", User(u))
}

type Token struct {
	ID    int64         `gorm:"primarykey"`
	Token *oauth2.Token `gorm:"serializer:json"`
}

type server struct {
	db   *gorm.DB
	bot  *tele.Bot
	auth *giraauth.Client

	mu sync.Mutex
	// tokenSources is a map of user ID to token source.
	// It's used to cache token sources, also to persist one instance of token source per user due to locking.
	tokenSouces map[int64]*tokenSource
}

var (
	adminID = flag.Int64("admin-id", 111504781, "admin user ID")
	dbPath  = flag.String("db-path", "girabot.db", "path to sqlite database")
)

func main() {
	flag.Parse()

	s := server{
		auth:        giraauth.New(http.DefaultClient),
		tokenSouces: map[int64]*tokenSource{},
	}

	// open DB
	db, err := gorm.Open(sqlite.Open(*dbPath), &gorm.Config{})
	if err != nil {
		log.Fatal(err)
	}
	if err := db.AutoMigrate(&User{}, &Token{}); err != nil {
		log.Fatal(err)
	}

	s.db = db

	// create bot
	b, err := tele.NewBot(tele.Settings{
		Token:   os.Getenv("TOKEN"),
		Poller:  &tele.LongPoller{Timeout: 10 * time.Second},
		OnError: s.onError,
	})
	if err != nil {
		log.Fatal(err)
	}

	s.bot = b

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt)

	go func() {
		<-done
		log.Println("stopping bot")
		b.Stop()

		d, _ := db.DB()
		_ = d.Close()
	}()

	// register middlewares and handlers
	b.Use(middleware.Recover())
	b.Use(s.addCustomContext)

	b.Handle("/start", wrapHandler((*customContext).handleStart))
	b.Handle("/login", wrapHandler((*customContext).handleLogin))
	b.Handle(tele.OnText, wrapHandler((*customContext).handleText))

	b.Handle("/debug", wrapHandler((*customContext).handleDebug), allowlist(*adminID))
	b.Handle("\f"+btnKeyTypeRetryDebug, wrapHandler((*customContext).handleDebugRetry), allowlist(*adminID))

	authed := b.Group()
	authed.Use(s.checkLoggedIn)

	authed.Handle("/help", wrapHandler((*customContext).handleHelp))
	authed.Handle("/status", wrapHandler((*customContext).handleStatus))
	authed.Handle(tele.OnLocation, wrapHandler((*customContext).handleLocation))
	authed.Handle("/rate", wrapHandler((*customContext).handleSendRateMsg))

	authed.Handle("/test", wrapHandler((*customContext).handleLocationTest), allowlist(*adminID))

	authed.Handle(&btnFavorites, wrapHandler((*customContext).handleShowFavorites))
	authed.Handle(&btnStatus, wrapHandler((*customContext).handleStatus))
	authed.Handle(&btnHelp, wrapHandler((*customContext).handleHelp))
	authed.Handle(&btnFeedback, wrapHandler((*customContext).handleFeedback))

	authed.Handle("\f"+btnKeyTypeStation, wrapHandler((*customContext).handleStation))
	authed.Handle("\f"+btnKeyTypeStationNextPage, wrapHandler((*customContext).handleStationNextPage))
	authed.Handle("\f"+btnKeyTypeBike, wrapHandler((*customContext).handleTapBike))
	authed.Handle("\f"+btnKeyTypeBikeUnlock, wrapHandler((*customContext).handleUnlockBike))
	authed.Handle("\f"+btnKeyTypeCloseMenu, wrapHandler((*customContext).deleteCallbackMessageWithReply))
	authed.Handle("\f"+btnKeyTypeCloseMenuKeepReply, wrapHandler((*customContext).deleteCallbackMessage))

	authed.Handle("\f"+btnKeyTypeAddFav, wrapHandler((*customContext).handleAddFavorite))
	authed.Handle("\f"+btnKeyTypeRemoveFav, wrapHandler((*customContext).handleRemoveFavorite))
	authed.Handle("\f"+btnKeyTypeRenameFav, wrapHandler((*customContext).handleRenameFavorite))

	authed.Handle("\f"+btnKeyTypeRateStar, wrapHandler((*customContext).handleRateStar))
	authed.Handle("\f"+btnKeyTypeRateAddText, wrapHandler((*customContext).handleRateAddText))
	authed.Handle("\f"+btnKeyTypeRateSubmit, wrapHandler((*customContext).handleRateSubmit))

	authed.Handle("\f"+btnKeyTypePayPoints, wrapHandler((*customContext).handlePayPoints))
	authed.Handle("\f"+btnKeyTypePayMoney, wrapHandler((*customContext).handlePayMoney))

	go s.refreshTokensWatcher()
	s.loadActiveTrips()

	log.Println("bot start")
	b.Start()
}

type customContext struct {
	tele.Context

	ctx context.Context

	s    *server
	user *User
	gira *gira.Client
}

// addCustomContext is a middleware that wraps telebot context to custom context,
// which includes gira client and user model.
// It also saves updated user model to database.
func (s *server) addCustomContext(next tele.HandlerFunc) tele.HandlerFunc {
	return func(c tele.Context) error {
		var u User
		res := s.db.First(&u, c.Sender().ID)
		if errors.Is(res.Error, gorm.ErrRecordNotFound) {
			log.Printf("user %d not found, creating", c.Sender().ID)

			u.ID = c.Sender().ID
			u.CreatedAt = time.Now()
			u.TGUsername = c.Sender().Username
			u.TGName = c.Sender().FirstName + " " + c.Sender().LastName
			u.Favorites = make(map[gira.StationSerial]string)

			res = s.db.Create(&u)
			if res.Error != nil {
				return res.Error
			}
		}

		defer func() {
			log.Println("saving user", filteredUser(u))
			// update user in database with changes from handler
			if err := s.db.Save(&u).Error; err != nil {
				log.Println("error saving user:", err)
			}
		}()

		log.Printf("bot call, action: '%s', user: %+v", getAction(c, u), filteredUser(u))

		ctx, cancel := s.newCustomContext(c, &u)
		defer cancel()
		return next(ctx)
	}
}

func (s *server) newCustomContext(c tele.Context, u *User) (*customContext, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

	girac := gira.New(oauth2.NewClient(ctx, s.getTokenSource(u.ID)))

	return &customContext{
		Context: c,
		ctx:     ctx,
		s:       s,
		user:    u,
		gira:    girac,
	}, cancel
}

func (s *server) onError(err error, c tele.Context) {
	var u User
	username := "?"

	if c != nil && c.Chat() != nil {
		s.db.First(&u, c.Chat().ID)
		username = c.Sender().Username
		if username == "" {
			username = strconv.Itoa(int(c.Sender().ID))
		}
	}

	msg := fmt.Sprintf("recovered error from @%v (%v): %+v", username, getAction(c, u), err)
	log.Println("bot:", msg)

	if _, err := s.bot.Send(tele.ChatID(*adminID), msg); err != nil {
		log.Println("bot: error sending recovered error:", err)
	}

	if u.ID != 0 && u.ID != *adminID {
		msg := fmt.Sprintf(
			"Internal error: %v.\nBot developer has been notified.",
			err,
		)
		if err := c.Send(msg); err != nil {
			log.Println("bot: error sending recovered error to user:", err)
		}
	}
}

func getAction(c tele.Context, u User) string {
	// user might be of zero value if it's not in database
	if c == nil {
		return "<nil>"
	}

	if c.Callback() != nil {
		return fmt.Sprintf("cb: uniq:%s, data:%s", c.Callback().Unique, c.Callback().Data)
	}
	if c.Message() == nil {
		return fmt.Sprintf("<weird upd: %+v>", c.Update())
	}
	if c.Message().Location != nil {
		return "<location>"
	}

	// do not send PII
	if u.State == UserStateWaitingForEmail {
		return "<email>"
	}
	if u.State == UserStateWaitingForPassword {
		return "<password>"
	}

	return c.Text()
}

func (s *server) refreshTokensWatcher() {
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt)

	for {
		select {
		case <-time.After(time.Hour + time.Duration(rand.Intn(300))*time.Second):
			log.Println("refreshing tokens")
			var tokens []Token
			if err := s.db.Find(&tokens).Error; err != nil {
				s.bot.OnError(fmt.Errorf("error getting tokens for refresh: %v", err), nil)
				continue
			}

			for _, tok := range tokens {
				// Refresh key is used to get new access key, so we refresh it if it's about to expire.
				// Access key expiry is 2 minutes, refresh key expiry is 7 days
				// It's easier to grab saved access token expiry than to parse JWT and get issued at.
				if time.Since(tok.Token.Expiry) < 6*24*time.Hour {
					continue
				}

				log.Println("refreshing token for", tok.ID)
				_, err := s.getTokenSource(tok.ID).Token()
				if err != nil {
					s.bot.OnError(fmt.Errorf("refreshing token for %d: %v", tok.ID, err), nil)
					continue
				}
			}
		case <-done:
			return
		}
	}
}

func (s *server) loadActiveTrips() {
	log.Println("loading active trips")
	var users []User
	if err := s.db.Find(&users).Error; err != nil {
		log.Fatalf("error getting users for active trip load: %v", err)
	}

	for _, u := range users {
		u := u
		if u.CurrentTripCode != "" && !u.CurrentTripRateAwaiting {
			log.Printf("starting active trip watch for %d", u.ID)
			// empty context update, we are not using any shorthands in watchActiveTrip
			c, cancel := s.newCustomContext(s.bot.NewContext(tele.Update{}), &u)
			go func() {
				defer cancel()
				if err := c.watchActiveTrip(false); err != nil {
					s.bot.OnError(fmt.Errorf("watching active trip: %v", err), c)
				}
			}()
		}
	}
}

// getTokenSource returns token source for user. It returns cached token source if it exists.
func (s *server) getTokenSource(uid int64) oauth2.TokenSource {
	s.mu.Lock()
	defer s.mu.Unlock()

	if ts, ok := s.tokenSouces[uid]; ok {
		return ts
	}

	s.tokenSouces[uid] = &tokenSource{
		db:   s.db,
		auth: s.auth,
		uid:  uid,
	}
	return s.tokenSouces[uid]
}

func (c *customContext) getTokenSource() oauth2.TokenSource {
	return c.s.getTokenSource(c.user.ID)
}

// tokenSource is an oauth2 token source that saves token to database.
// It also refreshes token if it's invalid. It's safe for concurrent use.
type tokenSource struct {
	db   *gorm.DB
	auth *giraauth.Client
	uid  int64

	mu sync.Mutex
}

func (t *tokenSource) Token() (*oauth2.Token, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	var tok Token
	if err := t.db.First(&tok, t.uid).Error; err != nil {
		return nil, err
	}

	l := log.New(os.Stderr, fmt.Sprintf("tokenSource[uid:%d] ", t.uid), log.LstdFlags)

	if tok.Token.Valid() {
		l.Printf("token is valid")
		return tok.Token, nil
	}

	l.Printf("token is invalid, refreshing")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	newToken, err := t.auth.Refresh(ctx, tok.Token.RefreshToken)
	if err != nil {
		l.Printf("refresh error: %v", err)
		return nil, err
	}
	l.Printf("refreshed ok")

	tok.Token = newToken
	if err := t.db.Save(&tok).Error; err != nil {
		l.Printf("save error: %v", err)
		return nil, err
	}

	return newToken, nil
}

// wrapHandler wraps handler that accepts custom context to handler that accepts telebot context.
func wrapHandler(f func(cc *customContext) error) func(tele.Context) error {
	return func(c tele.Context) error {
		return f(c.(*customContext))
	}
}

func allowlist(chats ...int64) tele.MiddlewareFunc {
	return func(next tele.HandlerFunc) tele.HandlerFunc {
		return middleware.Restrict(middleware.RestrictConfig{
			Chats: chats,
			In:    next,
			Out: func(c tele.Context) error {
				log.Printf("bot: user not in allowlist: %+v", c.Sender())
				return nil
			},
		})(next)
	}
}
