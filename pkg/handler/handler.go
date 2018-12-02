package handler

import (
	"encoding/json"
	"io/ioutil"
	"net/http"
	"path"
	"strings"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
	patreon "github.com/mxpv/patreon-go"
	itunes "github.com/mxpv/podcast"
	"golang.org/x/oauth2"

	"github.com/mxpv/podsync/pkg/api"
	"github.com/mxpv/podsync/pkg/config"
	"github.com/mxpv/podsync/pkg/session"

	log "github.com/sirupsen/logrus"
)

const (
	maxHashIDLength = 16
)

type feedService interface {
	CreateFeed(req *api.CreateFeedRequest, identity *api.Identity) (string, error)
	BuildFeed(hashID string) (*itunes.Podcast, error)
	GetMetadata(hashId string) (*api.Metadata, error)
	Downgrade(patronID string, featureLevel int) error
}

type patreonService interface {
	Hook(pledge *patreon.Pledge, event string) error
	GetFeatureLevelByID(patronID string) int
	GetFeatureLevelFromAmount(amount int) int
}

type handler struct {
	feed    feedService
	cfg     *config.AppConfig
	oauth2  oauth2.Config
	patreon patreonService
}

func (h handler) index(c *gin.Context) {
	identity, err := session.GetIdentity(c)
	if err != nil {
		identity = &api.Identity{}
	}

	c.HTML(http.StatusOK, "index.html", identity)
}

func (h handler) login(c *gin.Context) {
	state, err := session.SetState(c)
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}

	authURL := h.oauth2.AuthCodeURL(state)
	c.Redirect(http.StatusFound, authURL)
}

func (h handler) logout(c *gin.Context) {
	session.Clear(c)

	c.Redirect(http.StatusFound, "/")
}

func (h handler) patreonCallback(c *gin.Context) {
	// Validate session state
	if session.GetSetate(c) != c.Query("state") {
		c.String(http.StatusUnauthorized, "invalid state")
		return
	}

	// Exchange code with tokens
	token, err := h.oauth2.Exchange(c.Request.Context(), c.Query("code"))
	if err != nil {
		c.String(http.StatusBadRequest, err.Error())
		return
	}

	// Create Patreon client
	tc := h.oauth2.Client(c.Request.Context(), token)
	client := patreon.NewClient(tc)

	// Query user info from Patreon
	user, err := client.FetchUser()
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}

	// Determine feature level
	level := h.patreon.GetFeatureLevelByID(user.Data.ID)

	identity := &api.Identity{
		UserId:       user.Data.ID,
		FullName:     user.Data.Attributes.FullName,
		Email:        user.Data.Attributes.Email,
		ProfileURL:   user.Data.Attributes.URL,
		FeatureLevel: level,
	}

	session.SetIdentity(c, identity)
	c.Redirect(http.StatusFound, "/")
}

func (h handler) robots(c *gin.Context) {
	c.String(http.StatusOK, `User-agent: *
Allow: /$
Disallow: /
Host: www.podsync.net`)
}

func (h handler) ping(c *gin.Context) {
	c.String(http.StatusOK, "ok")
}

func (h handler) create(c *gin.Context) {
	req := &api.CreateFeedRequest{}

	if err := c.BindJSON(req); err != nil {
		c.JSON(badRequest(err))
		return
	}

	identity, err := session.GetIdentity(c)
	if err != nil {
		c.JSON(internalError(err))
		return
	}

	// Check feature level again if user deleted pledge by still logged in
	identity.FeatureLevel = h.patreon.GetFeatureLevelByID(identity.UserId)

	hashId, err := h.feed.CreateFeed(req, identity)
	if err != nil {
		c.JSON(internalError(err))
		return
	}

	c.JSON(http.StatusOK, gin.H{"id": hashId})
}

func (h handler) getFeed(c *gin.Context) {
	hashID := c.Request.URL.Path[1:]
	if hashID == "" || len(hashID) > maxHashIDLength {
		c.String(http.StatusBadRequest, "invalid feed id")
		return
	}

	if strings.HasSuffix(hashID, ".xml") {
		hashID = strings.TrimSuffix(hashID, ".xml")
	}

	podcast, err := h.feed.BuildFeed(hashID)
	if err != nil {
		log.WithError(err).WithField("hash_id", hashID).Error("failed to build feed")

		code := http.StatusInternalServerError
		if err == api.ErrNotFound {
			code = http.StatusNotFound
		} else if err == api.ErrQuotaExceeded {
			code = http.StatusTooManyRequests
		}

		c.String(code, err.Error())
		return
	}

	c.Data(http.StatusOK, "application/rss+xml; charset=UTF-8", podcast.Bytes())
}

func (h handler) metadata(c *gin.Context) {
	hashId := c.Param("hashId")
	if hashId == "" || len(hashId) > maxHashIDLength {
		c.String(http.StatusBadRequest, "invalid feed id")
		return
	}

	feed, err := h.feed.GetMetadata(hashId)
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}

	c.JSON(http.StatusOK, feed)
}

func (h handler) webhook(c *gin.Context) {
	// Read body to byte array in order to verify signature first
	body, err := ioutil.ReadAll(c.Request.Body)
	if err != nil {
		log.WithError(err).Error("failed to read webhook request")
		c.Status(http.StatusBadRequest)
		return
	}

	// Verify signature
	signature := c.GetHeader(patreon.HeaderSignature)
	valid, err := patreon.VerifySignature(body, h.cfg.PatreonWebhooksSecret, signature)
	if err != nil {
		log.WithError(err).Error("failed to verify signature")
		c.Status(http.StatusBadRequest)
		return
	}

	if !valid {
		log.Errorf("webhooks signatures are not equal (header: %s)", signature)
		c.Status(http.StatusUnauthorized)
		return
	}

	// Get event name
	eventName := c.GetHeader(patreon.HeaderEventType)
	if eventName == "" {
		log.Error("event name header is empty")
		c.Status(http.StatusBadRequest)
		return
	}

	pledge := &patreon.WebhookPledge{}
	if err := json.Unmarshal(body, pledge); err != nil {
		log.WithError(err).Error("failed to unmarshal pledge")
		c.JSON(badRequest(err))
		return
	}

	if err := h.patreon.Hook(&pledge.Data, eventName); err != nil {
		log.WithError(err).WithFields(log.Fields{
			"user_id":      pledge.Data.Relationships.Patron.Data.ID,
			"pledge_id":    pledge.Data.ID,
			"pledge_event": eventName,
		}).Error("failed to process patreon event")

		// Don't return any errors to Patreon, otherwise subsequent notifications will be blocked.
		return
	}

	patronID := pledge.Data.Relationships.Patron.Data.ID

	if eventName == patreon.EventUpdatePledge {
		newLevel := h.patreon.GetFeatureLevelFromAmount(pledge.Data.Attributes.AmountCents)
		if err := h.feed.Downgrade(patronID, newLevel); err != nil {
			return
		}
	} else if eventName == patreon.EventDeletePledge {
		if err := h.feed.Downgrade(patronID, api.DefaultFeatures); err != nil {
			return
		}
	}

	log.Infof("sucessfully processed patreon event %s (%s)", pledge.Data.ID, eventName)
}

func New(feed feedService, support patreonService, cfg *config.AppConfig) http.Handler {
	r := gin.New()
	r.Use(gin.Recovery())

	store := sessions.NewCookieStore([]byte(cfg.CookieSecret))
	r.Use(sessions.Sessions("podsync", store))

	// Static files + HTML

	log.Printf("using assets path: %s", cfg.AssetsPath)
	if cfg.AssetsPath != "" {
		r.Static("/assets", cfg.AssetsPath)
	}

	log.Printf("using templates path: %s", cfg.TemplatesPath)
	if cfg.TemplatesPath != "" {
		r.LoadHTMLGlob(path.Join(cfg.TemplatesPath, "*.html"))
	}

	h := handler{
		feed:    feed,
		patreon: support,
		cfg:     cfg,
	}

	// OAuth 2 configuration

	h.oauth2 = oauth2.Config{
		ClientID:     cfg.PatreonClientId,
		ClientSecret: cfg.PatreonSecret,
		RedirectURL:  cfg.PatreonRedirectURL,
		Scopes:       []string{"users", "pledges-to-me", "my-campaign"},
		Endpoint: oauth2.Endpoint{
			AuthURL:  patreon.AuthorizationURL,
			TokenURL: patreon.AccessTokenURL,
		},
	}

	// Handlers

	r.GET("/", h.index)
	r.GET("/login", h.login)
	r.GET("/logout", h.logout)
	r.GET("/patreon", h.patreonCallback)
	r.GET("/robots.txt", h.robots)

	r.GET("/api/ping", h.ping)
	r.POST("/api/create", h.create)
	r.GET("/api/metadata/:hashId", h.metadata)
	r.POST("/api/webhooks", h.webhook)

	r.NoRoute(h.getFeed)

	return r
}

func badRequest(err error) (int, interface{}) {
	return http.StatusBadRequest, gin.H{"error": err.Error()}
}

func internalError(err error) (int, interface{}) {
	log.Printf("server error: %v", err)
	return http.StatusInternalServerError, gin.H{"error": err.Error()}
}
