package core

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/panjf2000/ants/v2"
	log "github.com/sirupsen/logrus"
	"github.com/star-39/moe-sticker-bot/pkg/config"
)

func InitWebAppServer() {
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	webapp := r.Group("/webapp")
	{
		//Group: /webapp
		webapp.StaticFS("/edit", http.Dir(config.Config.WebappDataDir))
		webapp.Static("/data", filepath.Join(config.Config.WebappDataDir, "data"))
		api := webapp.Group("/api")
		{
			//Group: /webapp/api
			api.POST("/initData", apiInitData)
			api.GET("/ss", apiSS)
			api.POST("/edit/result", apiEditResult)
			api.POST("/edit/move", apiEditMove)
		}
	}

	go func() {
		err := r.RunTLS(config.Config.WebappListenAddr,
			config.Config.WebappCert,
			config.Config.WebappPrivkey)
		if err != nil {
			log.Fatalln("WebApp: Gin RunTLS failed! Check your certs and addr or disable webapp.", err)
		}
	}()
}

type webappStickerObject struct {
	//Sticker index with offset of +1
	Id int `json:"id"`
	//Sticker emojis.
	Emoji string `json:"emoji"`
	//Sticker emoji changed on front-end.
	EmojiChanged bool `json:"emoji_changed"`
	//Sticker image URL.
	Surl string `json:"surl"`
	//StickerSet Name
	SSName string `json:"ssname"`
}

// <- ?uid&query_id
// -------------------------------------------
// -> [{"index", "emoji", "surl"}, ...]
// -------------------------------------------
// id starts from 1 !!!!
// surl might be 404 when preparing stickers.
func apiSS(c *gin.Context) {
	uid := c.Query("uid")
	qid := c.Query("qid")
	ud, err := checkGetUd(uid, qid)
	if err != nil {
		c.String(http.StatusBadRequest, err.Error())
		return
	}
	sObjList := []webappStickerObject{}
	for i, s := range ud.stickerData.stickerSet.Stickers {
		surl, _ := url.JoinPath(config.Config.WebappUrl, "data", s.SetName, s.FileID+".webp")
		sObjList = append(sObjList, webappStickerObject{
			SSName: ud.stickerData.stickerSet.Name,
			Id:     i + 1,
			Emoji:  s.Emoji,
			Surl:   surl,
		})
	}
	jSMap, err := json.Marshal(sObjList)
	if err != nil {
		log.Errorln("json marshal sMap in apiSS error!")
		c.String(http.StatusInternalServerError, "json marshal sMap in apiSS error!")
		return
	}
	c.String(http.StatusOK, string(jSMap))
}

// <- ?qid&qid&sha256sum  [{"index", "emoji", "surl"}, ...]
// -------------------------------------------
// -> STATUS
func apiEditResult(c *gin.Context) {
	uid := c.Query("uid")
	qid := c.Query("qid")
	sum := c.Query("sha256sum")
	body, _ := io.ReadAll(c.Request.Body)
	if !validateSHA256(body, sum) {
		c.String(http.StatusBadRequest, "bad result csum!")
		return
	}
	if string(body) == "" {
		//user did nothing
		return
	}
	sObjs := []webappStickerObject{}
	err := json.Unmarshal(body, &sObjs)
	if err != nil {
		c.String(http.StatusBadRequest, "no items")
	}
	ud, err := checkGetUd(uid, qid)
	if err != nil {
		c.String(http.StatusBadRequest, err.Error())
		return
	}
	// err = checkSOrder(ud, sObjs)
	// if err != nil {
	// 	log.Warn(err)
	// 	c.String(http.StatusInternalServerError, err.Error())
	// 	return
	// }

	c.String(http.StatusOK, "")
	ud.udSetState(ST_PROCESSING)

	go commitEmojiChange(ud, sObjs)
}

func commitEmojiChange(ud *UserData, sObjs []webappStickerObject) error {
	ud.webAppWorkerPool.ReleaseTimeout(10 * time.Second)
	notificationSent := false
	for i, s := range ud.stickerData.stickerSet.Stickers {
		if !sObjs[i].EmojiChanged {
			continue
		}
		newEmoji := findEmojis(sObjs[i].Emoji)
		if newEmoji == "" {
			log.Warn("webapp: ignored one invalid emoji.")
			continue
		}
		base := filepath.Base(sObjs[i].Surl)
		fid := strings.TrimSuffix(base, filepath.Ext(base))
		if newEmoji == findEmojis(s.Emoji) {
			log.Debugln("emoji not actually changed", i)
			continue
		}
		log.Debugln("Old:", i, s.Emoji, s.FileID)
		log.Debugln("New", i, newEmoji, sObjs[i].Surl)
		if !notificationSent {
			sendEditingEmoji(ud.lastContext)
			notificationSent = true
		}
		err := editStickerEmoji(newEmoji, i, fid, ud)
		if err != nil {
			sendFatalError(err, ud.lastContext)
			cleanUserDataAndDir(ud.lastContext.Sender().ID)
		}
	}
	if notificationSent {
		sendSEditOK(ud.lastContext)
		sendSFromSS(ud.lastContext)
		endSession(ud.lastContext)
	}
	return nil
}

// func checkSOrder(ud *UserData, sObjs []webappStickerObject) error {
// 	ud.webAppWorkerPool.ReleaseTimeout(10 * time.Second)
// 	ss, err := b.StickerSet(ud.stickerData.stickerSet.Name)
// 	if err != nil {
// 		return err
// 	}
// 	for i, s := range ss.Stickers {
// 		base := filepath.Base(sObjs[i].Surl)
// 		fid := strings.TrimSuffix(base, filepath.Ext(base))
// 		if s.FileID != fid {
// 			ud.lastContext.Send(&s)
// 			log.Warnln("ss mismatch", i, s.FileID, fid)
// 			return errors.New("ss order mismatch")
// 		}
// 	}
// 	return nil
// }

// <- ?uid&qid POST_FORM:{"oldIndex", "newIndex"}
// -------------------------------------------
// -> STATUS
func apiEditMove(c *gin.Context) {
	uid := c.Query("uid")
	qid := c.Query("qid")
	oldIndex, _ := strconv.Atoi(c.PostForm("oldIndex"))
	newIndex, _ := strconv.Atoi(c.PostForm("newIndex"))
	ud, err := checkGetUd(uid, qid)
	if err != nil {
		c.String(http.StatusBadRequest, err.Error())
		return
	}
	smo := &StickerMoveObject{
		wg:       sync.WaitGroup{},
		sd:       ud.stickerData,
		oldIndex: oldIndex,
		newIndex: newIndex,
	}
	smo.wg.Add(1)
	ud.webAppWorkerPool.Invoke(smo)
	smo.wg.Wait()
	if smo.err != nil {
		c.String(http.StatusInternalServerError, smo.err.Error())
		return
	}
}

func apiInitData(c *gin.Context) {
	//We must verify the initData before using it
	queryID := c.PostForm("query_id")
	authDate := c.PostForm("auth_date")
	user := c.PostForm("user")
	hash := c.PostForm("hash")
	dataCheckString := strings.Join([]string{
		"auth_date=" + authDate,
		"query_id=" + queryID,
		"user=" + user}, "\n")
	if !validateHMAC(dataCheckString, hash) {
		log.Warning("WebApp DCS HMAC failed, corrupt or attack?")
		c.String(http.StatusBadRequest, "data_check_string HMAC validation failed!!")
		return
	}
	c.String(http.StatusOK, "data_check_string ok")
	log.Debug("WebApp initData DCS HMAC OK.")
	webAppUser := &WebAppUser{}
	err := json.Unmarshal([]byte(user), webAppUser)
	if err != nil {
		log.Error("json unmarshal webappuser error.")
		return
	}

	ud, err := GetUd(strconv.Itoa(webAppUser.Id))
	if err != nil {
		log.Warning("Bad webapp user init, not in state?")
		c.String(http.StatusBadRequest, "bad webapp user!")
		return
	}

	ud.webAppWorkerPool, _ = ants.NewPoolWithFunc(1, wSubmitSMove)
	ud.webAppQID = queryID

	// ud.udSetState(ST_WAIT_WEBAPP)
	c.String(http.StatusOK, "webapp init ok")
}

// Telegram WebApp Regulation.
func validateHMAC(dataCheckString string, hash string) bool {
	// This calculated secret will be used to "decrypt" DCS
	h := hmac.New(sha256.New, []byte("WebAppData"))
	h.Write([]byte(config.Config.BotToken))
	secretByte := h.Sum(nil)

	h = hmac.New(sha256.New, secretByte)
	h.Write([]byte(dataCheckString))
	dcsHash := fmt.Sprintf("%x", h.Sum(nil))
	return hash == dcsHash
}

func validateSHA256(dataToCheck []byte, hash string) bool {
	h := sha256.New()
	h.Write(dataToCheck)
	csum := fmt.Sprintf("%x", h.Sum(nil))
	return hash == csum
}

func checkGetUd(uid string, qid string) (*UserData, error) {
	ud, err := GetUd(uid)
	if err != nil {
		return nil, errors.New("no such user")
	}
	if ud.webAppQID != qid {
		return nil, errors.New("qid not valid")
	}
	return ud, nil
}