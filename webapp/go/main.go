package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bytedance/sonic"
	"github.com/samber/lo"
	"io/ioutil"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/motoki317/sc"

	"github.com/dgrijalva/jwt-go"
	"github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/labstack/gommon/log"
)

const (
	sessionName                 = "isucondition_go"
	conditionLimit              = 20
	frontendContentsPath        = "../public"
	jiaJWTSigningKeyPath        = "../ec256-public.pem"
	defaultIconFilePath         = "../NoImage.jpg"
	isuImagesPath               = "../isu-images"
	defaultJIAServiceURL        = "http://localhost:5000"
	mysqlErrNumDuplicateEntry   = 1062
	conditionLevelInfo          = "info"
	conditionLevelWarning       = "warning"
	conditionLevelCritical      = "critical"
	scoreConditionLevelInfo     = 3
	scoreConditionLevelWarning  = 2
	scoreConditionLevelCritical = 1
)

var (
	db                  *sqlx.DB
	sessionStore        sessions.Store
	mySQLConnectionData *MySQLConnectionEnv

	jiaJWTSigningKey *ecdsa.PublicKey

	postIsuConditionTargetBaseURL string // JIAへのactivate時に登録する，ISUがconditionを送る先のURL
)

type Config struct {
	Name string `db:"name"`
	URL  string `db:"url"`
}

type Isu struct {
	ID         int       `db:"id" json:"id"`
	JIAIsuUUID string    `db:"jia_isu_uuid" json:"jia_isu_uuid"`
	Name       string    `db:"name" json:"name"`
	Character  string    `db:"character" json:"character"`
	JIAUserID  string    `db:"jia_user_id" json:"-"`
	CreatedAt  time.Time `db:"created_at" json:"-"`
	UpdatedAt  time.Time `db:"updated_at" json:"-"`
}

type IsuFromJIA struct {
	Character string `json:"character"`
}

type GetIsuListResponse struct {
	ID                 int                      `json:"id"`
	JIAIsuUUID         string                   `json:"jia_isu_uuid"`
	Name               string                   `json:"name"`
	Character          string                   `json:"character"`
	LatestIsuCondition *GetIsuConditionResponse `json:"latest_isu_condition"`
}

type IsuCondition struct {
	ID             int       `db:"id"`
	JIAIsuUUID     string    `db:"jia_isu_uuid"`
	Timestamp      time.Time `db:"timestamp"`
	IsSitting      bool      `db:"is_sitting"`
	Condition      string    `db:"condition"`
	ConditionLevel string    `db:"condition_level"`
	Message        string    `db:"message"`
	CreatedAt      time.Time `db:"created_at"`
}

type MySQLConnectionEnv struct {
	Host     string
	Port     string
	User     string
	DBName   string
	Password string
}

type InitializeRequest struct {
	JIAServiceURL string `json:"jia_service_url"`
}

type InitializeResponse struct {
	Language string `json:"language"`
}

type GetMeResponse struct {
	JIAUserID string `json:"jia_user_id"`
}

type GraphResponse struct {
	StartAt             int64           `json:"start_at"`
	EndAt               int64           `json:"end_at"`
	Data                *GraphDataPoint `json:"data"`
	ConditionTimestamps []int64         `json:"condition_timestamps"`
}

type GraphDataPoint struct {
	Score      int                  `json:"score"`
	Percentage ConditionsPercentage `json:"percentage"`
}

type ConditionsPercentage struct {
	Sitting      int `json:"sitting"`
	IsBroken     int `json:"is_broken"`
	IsDirty      int `json:"is_dirty"`
	IsOverweight int `json:"is_overweight"`
}

type GraphDataPointWithInfo struct {
	JIAIsuUUID          string
	StartAt             time.Time
	Data                GraphDataPoint
	ConditionTimestamps []int64
}

type GetIsuConditionResponse struct {
	JIAIsuUUID     string `json:"jia_isu_uuid"`
	IsuName        string `json:"isu_name"`
	Timestamp      int64  `json:"timestamp"`
	IsSitting      bool   `json:"is_sitting"`
	Condition      string `json:"condition"`
	ConditionLevel string `json:"condition_level"`
	Message        string `json:"message"`
}

type TrendResponse struct {
	Character string            `json:"character"`
	Info      []*TrendCondition `json:"info"`
	Warning   []*TrendCondition `json:"warning"`
	Critical  []*TrendCondition `json:"critical"`
}

type TrendCondition struct {
	ID        int   `json:"isu_id"`
	Timestamp int64 `json:"timestamp"`
}

type PostIsuConditionRequest struct {
	IsSitting bool   `json:"is_sitting"`
	Condition string `json:"condition"`
	Message   string `json:"message"`
	Timestamp int64  `json:"timestamp"`
}

type JIAServiceRequest struct {
	TargetBaseURL string `json:"target_base_url"`
	IsuUUID       string `json:"isu_uuid"`
}

func getEnv(key string, defaultValue string) string {
	val := os.Getenv(key)
	if val != "" {
		return val
	}
	return defaultValue
}

func NewMySQLConnectionEnv() *MySQLConnectionEnv {
	return &MySQLConnectionEnv{
		Host:     getEnv("MYSQL_HOST", "127.0.0.1"),
		Port:     getEnv("MYSQL_PORT", "3306"),
		User:     getEnv("MYSQL_USER", "isucon"),
		DBName:   getEnv("MYSQL_DBNAME", "isucondition"),
		Password: getEnv("MYSQL_PASS", "isucon"),
	}
}

func (mc *MySQLConnectionEnv) ConnectDB() (*sqlx.DB, error) {
	dsn := fmt.Sprintf("%v:%v@tcp(%v:%v)/%v?parseTime=true&loc=Asia%%2FTokyo&interpolateParams=true", mc.User, mc.Password, mc.Host, mc.Port, mc.DBName)
	return sqlx.Open("mysql", dsn)
}

func init() {
	sessionStore = sessions.NewCookieStore([]byte(getEnv("SESSION_KEY", "isucondition")))

	key, err := ioutil.ReadFile(jiaJWTSigningKeyPath)
	if err != nil {
		log.Fatalf("failed to read file: %v", err)
	}
	jiaJWTSigningKey, err = jwt.ParseECPublicKeyFromPEM(key)
	if err != nil {
		log.Fatalf("failed to parse ECDSA public key: %v", err)
	}
}

var benchstart atomic.Pointer[time.Time]

const benchtime = (60 + 20 + 60) * time.Second

func main() {
	//http.DefaultServeMux.Handle("/debug/fgprof", fgprof.Handler())
	go func() {
		log.Fatal(http.ListenAndServe(":6060", nil))
	}()

	e := echo.New()
	//e.Debug = true
	e.Logger.SetLevel(log.ERROR)

	//e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	e.POST("/initialize", postInitialize)

	e.POST("/api/auth", postAuthentication)
	e.POST("/api/signout", postSignout)
	e.GET("/api/user/me", getMe)
	e.GET("/api/isu", getIsuList)
	e.POST("/api/isu", postIsu)
	e.GET("/api/isu/:jia_isu_uuid", getIsuID)
	e.GET("/api/isu/:jia_isu_uuid/icon", getIsuIcon)
	e.GET("/api/isu/:jia_isu_uuid/graph", getIsuGraph)
	e.GET("/api/condition/:jia_isu_uuid", getIsuConditions)
	e.GET("/api/trend", getTrend)

	e.POST("/api/condition/:jia_isu_uuid", postIsuCondition)

	e.GET("/", getIndex)
	e.GET("/isu/:jia_isu_uuid", getIndex)
	e.GET("/isu/:jia_isu_uuid/condition", getIndex)
	e.GET("/isu/:jia_isu_uuid/graph", getIndex)
	e.GET("/register", getIndex)
	e.Static("/assets", frontendContentsPath+"/assets")

	mySQLConnectionData = NewMySQLConnectionEnv()

	var err error
	db, err = mySQLConnectionData.ConnectDB()
	if err != nil {
		e.Logger.Fatalf("failed to connect db: %v", err)
		return
	}
	db.SetMaxIdleConns(100)
	db.SetMaxOpenConns(100)
	defer db.Close()

	tmpTime := &time.Time{}
	*tmpTime = time.Now()
	benchstart.Store(tmpTime)
	go func() {
		for {
			time.Sleep(benchtime + 1*time.Second)
			//ベンチ中: now <= benchstart+benchtime
			if time.Now().Add(-benchtime).Before(*benchstart.Load()) {
				continue
			}
			var err error = db.Ping()
			if err != nil {
				e.Logger.Fatalf("db ping error %v", err)
				panic(err)
			}
		}
	}()

	postIsuConditionTargetBaseURL = os.Getenv("POST_ISUCONDITION_TARGET_BASE_URL")
	if postIsuConditionTargetBaseURL == "" {
		e.Logger.Fatalf("missing: POST_ISUCONDITION_TARGET_BASE_URL")
		return
	}

	serverPort := fmt.Sprintf(":%v", getEnv("SERVER_APP_PORT", "3000"))
	e.Logger.Fatal(e.Start(serverPort))
}

func jsonEncode(res any) []byte {
	b, err := sonic.Marshal(&res)
	if err != nil {
		panic(err)
	}

	return b
}

func stmtClose(stmt *sqlx.Stmt) {
	_ = stmt.Close()
}

var stmtCache = sc.NewMust(func(ctx context.Context, query string) (*sqlx.Stmt, error) {
	stmt, err := db.PreparexContext(ctx, query)
	if err != nil {
		return nil, err
	}
	runtime.SetFinalizer(stmt, stmtClose)
	return stmt, nil
}, 90*time.Second, 90*time.Second)

func dbExec(query string, args ...any) (sql.Result, error) {
	stmt, err := stmtCache.Get(context.Background(), query)
	if err != nil {
		return nil, err
	}
	return stmt.Exec(args...)
}

func dbGet(dest interface{}, query string, args ...interface{}) error {
	stmt, err := stmtCache.Get(context.Background(), query)
	if err != nil {
		return err
	}
	return stmt.Get(dest, args...)
}

func dbSelect(dest interface{}, query string, args ...interface{}) error {
	stmt, err := stmtCache.Get(context.Background(), query)
	if err != nil {
		return err
	}
	return stmt.Select(dest, args...)
}

func getSession(r *http.Request) (*sessions.Session, error) {
	session, err := sessionStore.Get(r, sessionName)
	if err != nil {
		return nil, err
	}
	return session, nil
}

var errUserNotFound = errors.New("user not found")

func retrieveUserExist(_ context.Context, jiaUserID string) (bool, error) {
	var count int
	err := dbGet(&count, "SELECT COUNT(*) FROM `user` WHERE `jia_user_id` = ?",
		jiaUserID)
	if err != nil {
		return false, err
	}

	if count == 0 {
		return false, errUserNotFound
	}

	return true, nil
}

var cacheUserExist = sc.NewMust[string, bool](retrieveUserExist, 300*time.Hour, 300*time.Hour)

func getUserIDFromSession(c echo.Context) (string, int, error) {
	session, err := getSession(c.Request())
	if err != nil {
		return "", http.StatusInternalServerError, fmt.Errorf("failed to get session: %v", err)
	}
	_jiaUserID, ok := session.Values["jia_user_id"]
	if !ok {
		return "", http.StatusUnauthorized, fmt.Errorf("no session")
	}

	jiaUserID := _jiaUserID.(string)
	//var count int
	//
	//err = dbGet(&count, "SELECT COUNT(*) FROM `user` WHERE `jia_user_id` = ?",
	//	jiaUserID)
	//if err != nil {
	//	return "", http.StatusInternalServerError, fmt.Errorf("db error: %v", err)
	//}
	//
	//if count == 0 {
	//	return "", http.StatusUnauthorized, fmt.Errorf("not found: user")
	//}

	exist, err := cacheUserExist.Get(context.Background(), jiaUserID)
	if err != nil {
		if errors.Is(err, errUserNotFound) {
			return "", http.StatusUnauthorized, fmt.Errorf("not found: user")
		}

		return "", http.StatusInternalServerError, fmt.Errorf("db error: %v", err)
	}

	if !exist {
		return "", http.StatusUnauthorized, fmt.Errorf("not found: user")
	}

	return jiaUserID, 0, nil
}

func getJIAServiceURL(tx *sqlx.Tx) string {
	var config Config
	err := tx.Get(&config, "SELECT * FROM `isu_association_config` WHERE `name` = ?", "jia_service_url")
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Print(err)
		}
		return defaultJIAServiceURL
	}
	return config.URL
}

// POST /initialize
// サービスを初期化
func postInitialize(c echo.Context) error {

	tmpTime := &time.Time{}
	*tmpTime = time.Now()
	benchstart.Store(tmpTime)

	cacheUserExist.Purge()

	if os.Getenv("SERVER_ID") == "s3" {
		cacheIsu.Purge()

		return c.JSONBlob(http.StatusOK, jsonEncode(InitializeResponse{
			Language: "go",
		}))
	}

	var request InitializeRequest
	err := c.Bind(&request)
	if err != nil {
		return c.String(http.StatusBadRequest, "bad request body")
	}

	reciver_err := make(chan error)
	go func() {
		defer close(reciver_err)
		req, err := http.NewRequest(http.MethodPost, "http://172.31.38.28/initialize", bytes.NewBuffer([]byte{}))
		if err != nil {
			reciver_err <- err
			return
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			reciver_err <- err
			return
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusOK {
			reciver_err <- fmt.Errorf("Initialize returned error: status code %v", res.StatusCode)
			return
		}
	}()
	cmd := exec.Command("../sql/init.sh")
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stderr
	err = cmd.Run()
	if err != nil {
		c.Logger().Errorf("exec init.sh error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	_, err = dbExec(
		"INSERT INTO `isu_association_config` (`name`, `url`) VALUES (?, ?) ON DUPLICATE KEY UPDATE `url` = VALUES(`url`)",
		"jia_service_url",
		request.JIAServiceURL,
	)
	if err != nil {
		c.Logger().Errorf("db error : %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	//condition_level
	conditions := []IsuCondition{}
	dbSelect(&conditions, "SELECT * FROM `isu_condition`")
	if err != nil {
		c.Logger().Errorf("db error : %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	for _, cond := range conditions {
		if !isValidConditionFormat(cond.Condition) {
			return c.NoContent(http.StatusInternalServerError)
		}
		cLevel, err := calculateConditionLevel(cond.Condition)
		if err != nil {
			return c.String(http.StatusBadRequest, "bad request body")
		}

		_, err = dbExec(
			"UPDATE `isu_condition` SET condition_level = ? WHERE jia_isu_uuid = ? AND timestamp = ?",
			cLevel, cond.JIAIsuUUID, cond.Timestamp)
		if err != nil {
			c.Logger().Errorf("db error: %v", err)
			return c.NoContent(http.StatusInternalServerError)
		}

	}
	cacheIsu.Purge()
	err = <-reciver_err
	if err != nil {
		c.Logger().Errorf("initialize error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	return c.JSONBlob(http.StatusOK, jsonEncode(InitializeResponse{
		Language: "go",
	}))
}

// POST /api/auth
// サインアップ・サインイン
func postAuthentication(c echo.Context) error {
	reqJwt := strings.TrimPrefix(c.Request().Header.Get("Authorization"), "Bearer ")

	token, err := jwt.Parse(reqJwt, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodECDSA); !ok {
			return nil, jwt.NewValidationError(fmt.Sprintf("unexpected signing method: %v", token.Header["alg"]), jwt.ValidationErrorSignatureInvalid)
		}
		return jiaJWTSigningKey, nil
	})
	if err != nil {
		switch err.(type) {
		case *jwt.ValidationError:
			return c.String(http.StatusForbidden, "forbidden")
		default:
			c.Logger().Error(err)
			return c.NoContent(http.StatusInternalServerError)
		}
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		c.Logger().Errorf("invalid JWT payload")
		return c.NoContent(http.StatusInternalServerError)
	}
	jiaUserIDVar, ok := claims["jia_user_id"]
	if !ok {
		return c.String(http.StatusBadRequest, "invalid JWT payload")
	}
	jiaUserID, ok := jiaUserIDVar.(string)
	if !ok {
		return c.String(http.StatusBadRequest, "invalid JWT payload")
	}

	_, err = dbExec("INSERT IGNORE INTO `user` (`jia_user_id`) VALUES (?)", jiaUserID)
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	cacheUserExist.Forget(jiaUserID)

	session, err := getSession(c.Request())
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	session.Values["jia_user_id"] = jiaUserID
	err = session.Save(c.Request(), c.Response())
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	return c.NoContent(http.StatusOK)
}

// POST /api/signout
// サインアウト
func postSignout(c echo.Context) error {
	_, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	session, err := getSession(c.Request())
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	session.Options = &sessions.Options{MaxAge: -1, Path: "/"}
	err = session.Save(c.Request(), c.Response())
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	return c.NoContent(http.StatusOK)
}

// GET /api/user/me
// サインインしている自分自身の情報を取得
func getMe(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	res := GetMeResponse{JIAUserID: jiaUserID}
	return c.JSONBlob(http.StatusOK, jsonEncode(res))
}

// GET /api/isu
// ISUの一覧を取得
func getIsuList(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	isuList := []Isu{}
	err = dbSelect(
		&isuList,
		"SELECT * FROM `isu` WHERE `jia_user_id` = ? ORDER BY `id` DESC",
		jiaUserID)
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	var conditions []*IsuCondition
	if len(isuList) > 0 {
		query := "SELECT * FROM `isu_condition` WHERE (jia_isu_uuid, timestamp) IN (SELECT jia_isu_uuid, MAX(timestamp) FROM isu_condition WHERE jia_isu_uuid IN (?) GROUP BY jia_isu_uuid)"
		jiaIsuIDs := lo.Map(isuList, func(isu Isu, _ int) any { return isu.JIAIsuUUID })
		query, args, err := sqlx.In(query, jiaIsuIDs)
		if err != nil {
			c.Logger().Errorf("sqlx.In error: %v", err)
			return c.NoContent(http.StatusInternalServerError)
		}
		err = db.Select(&conditions, query, args...)
		if err != nil {
			c.Logger().Errorf("db error: %v", err)
			return c.NoContent(http.StatusInternalServerError)
		}
	}
	conditionsMap := lo.SliceToMap(conditions, func(c *IsuCondition) (string, *IsuCondition) { return c.JIAIsuUUID, c })

	responseList := []GetIsuListResponse{}
	for _, isu := range isuList {
		lastCondition, ok := conditionsMap[isu.JIAIsuUUID]

		var formattedCondition *GetIsuConditionResponse
		if ok {
			conditionLevel, err := calculateConditionLevel(lastCondition.Condition)
			if err != nil {
				c.Logger().Error(err)
				return c.NoContent(http.StatusInternalServerError)
			}

			formattedCondition = &GetIsuConditionResponse{
				JIAIsuUUID:     lastCondition.JIAIsuUUID,
				IsuName:        isu.Name,
				Timestamp:      lastCondition.Timestamp.Unix(),
				IsSitting:      lastCondition.IsSitting,
				Condition:      lastCondition.Condition,
				ConditionLevel: conditionLevel,
				Message:        lastCondition.Message,
			}
		}

		res := GetIsuListResponse{
			ID:                 isu.ID,
			JIAIsuUUID:         isu.JIAIsuUUID,
			Name:               isu.Name,
			Character:          isu.Character,
			LatestIsuCondition: formattedCondition}
		responseList = append(responseList, res)
	}

	return c.JSONBlob(http.StatusOK, jsonEncode(responseList))
}

// POST /api/isu
// ISUを登録
func postIsu(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	useDefaultImage := false

	jiaIsuUUID := c.FormValue("jia_isu_uuid")
	isuName := c.FormValue("isu_name")
	fh, err := c.FormFile("image")
	if err != nil {
		if !errors.Is(err, http.ErrMissingFile) {
			return c.String(http.StatusBadRequest, "bad format: icon")
		}
		useDefaultImage = true
	}

	var image []byte

	if useDefaultImage {
		image, err = ioutil.ReadFile(defaultIconFilePath)
		if err != nil {
			c.Logger().Error(err)
			return c.NoContent(http.StatusInternalServerError)
		}
	} else {
		file, err := fh.Open()
		if err != nil {
			c.Logger().Error(err)
			return c.NoContent(http.StatusInternalServerError)
		}
		defer file.Close()

		image, err = ioutil.ReadAll(file)
		if err != nil {
			c.Logger().Error(err)
			return c.NoContent(http.StatusInternalServerError)
		}
	}

	err = os.WriteFile(filepath.Join(isuImagesPath, jiaIsuUUID), image, 0644)
	if err != nil {
		c.Logger().Errorf("write image file error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	tx, err := db.Beginx()
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	defer tx.Rollback()

	_, err = tx.Exec("INSERT INTO `isu` (`jia_isu_uuid`, `name`, `jia_user_id`) VALUES (?, ?, ?)",
		jiaIsuUUID, isuName, jiaUserID)
	if err != nil {
		mysqlErr, ok := err.(*mysql.MySQLError)

		if ok && mysqlErr.Number == uint16(mysqlErrNumDuplicateEntry) {
			return c.String(http.StatusConflict, "duplicated: isu")
		}

		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	targetURL := getJIAServiceURL(tx) + "/api/activate"
	body := JIAServiceRequest{postIsuConditionTargetBaseURL, jiaIsuUUID}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	reqJIA, err := http.NewRequest(http.MethodPost, targetURL, bytes.NewBuffer(bodyJSON))
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	reqJIA.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(reqJIA)
	if err != nil {
		c.Logger().Errorf("failed to request to JIAService: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	defer res.Body.Close()

	resBody, err := ioutil.ReadAll(res.Body)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	if res.StatusCode != http.StatusAccepted {
		c.Logger().Errorf("JIAService returned error: status code %v, message: %v", res.StatusCode, string(resBody))
		return c.String(res.StatusCode, "JIAService returned error")
	}

	var isuFromJIA IsuFromJIA
	err = json.Unmarshal(resBody, &isuFromJIA)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	_, err = tx.Exec("UPDATE `isu` SET `character` = ? WHERE  `jia_isu_uuid` = ?", isuFromJIA.Character, jiaIsuUUID)
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	var isu Isu
	err = tx.Get(
		&isu,
		"SELECT * FROM `isu` WHERE `jia_user_id` = ? AND `jia_isu_uuid` = ?",
		jiaUserID, jiaIsuUUID)
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	err = tx.Commit()
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	cacheIsu.Forget(jiaIsuUUID)

	return c.JSONBlob(http.StatusCreated, jsonEncode(isu))
}

// GET /api/isu/:jia_isu_uuid
// ISUの情報を取得
func getIsuID(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	jiaIsuUUID := c.Param("jia_isu_uuid")

	//var res Isu
	//err = dbGet(&res, "SELECT * FROM `isu` WHERE `jia_user_id` = ? AND `jia_isu_uuid` = ?",
	//	jiaUserID, jiaIsuUUID)
	//if err != nil {
	//	if errors.Is(err, sql.ErrNoRows) {
	//		return c.String(http.StatusNotFound, "not found: isu")
	//	}
	//
	//	c.Logger().Errorf("db error: %v", err)
	//	return c.NoContent(http.StatusInternalServerError)
	//}

	res, err := cacheIsu.Get(context.Background(), jiaIsuUUID)
	if err != nil {
		if errors.Is(err, errIsuNotFound) {
			return c.String(http.StatusNotFound, "not found: isu")
		}

		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	if res.JIAUserID != jiaUserID {
		return c.String(http.StatusNotFound, "not found: isu")
	}

	return c.JSONBlob(http.StatusOK, jsonEncode(res))
}

// GET /api/isu/:jia_isu_uuid/icon
// ISUのアイコンを取得
func getIsuIcon(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	jiaIsuUUID := c.Param("jia_isu_uuid")

	//var count int
	//err = dbGet(&count, "SELECT COUNT(*) FROM `isu` WHERE `jia_user_id` = ? AND `jia_isu_uuid` = ?",
	//	jiaUserID, jiaIsuUUID)
	//if err != nil {
	//	c.Logger().Errorf("db error: %v", err)
	//	return c.NoContent(http.StatusInternalServerError)
	//}
	//if count == 0 {
	//	return c.String(http.StatusNotFound, "not found: isu")
	//}

	isu, err := cacheIsu.Get(context.Background(), jiaIsuUUID)
	if err != nil {
		if errors.Is(err, errIsuNotFound) {
			return c.String(http.StatusNotFound, "not found: isu")
		}

		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	if isu.JIAUserID != jiaUserID {
		return c.String(http.StatusNotFound, "not found: isu")
	}

	c.Response().Header().Set("X-Accel-Redirect", "/isu-images/"+jiaIsuUUID)
	return c.NoContent(http.StatusOK)
}

// GET /api/isu/:jia_isu_uuid/graph
// ISUのコンディショングラフ描画のための情報を取得
func getIsuGraph(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	jiaIsuUUID := c.Param("jia_isu_uuid")
	datetimeStr := c.QueryParam("datetime")
	if datetimeStr == "" {
		return c.String(http.StatusBadRequest, "missing: datetime")
	}
	datetimeInt64, err := strconv.ParseInt(datetimeStr, 10, 64)
	if err != nil {
		return c.String(http.StatusBadRequest, "bad format: datetime")
	}
	date := time.Unix(datetimeInt64, 0).Truncate(time.Hour)

	//var count int
	//err = dbGet(&count, "SELECT COUNT(*) FROM `isu` WHERE `jia_user_id` = ? AND `jia_isu_uuid` = ?",
	//	jiaUserID, jiaIsuUUID)
	//if err != nil {
	//	c.Logger().Errorf("db error: %v", err)
	//	return c.NoContent(http.StatusInternalServerError)
	//}
	//if count == 0 {
	//	return c.String(http.StatusNotFound, "not found: isu")
	//}

	isu, err := cacheIsu.Get(context.Background(), jiaIsuUUID)
	if err != nil {
		if errors.Is(err, errIsuNotFound) {
			return c.String(http.StatusNotFound, "not found: isu")
		}

		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	if isu.JIAUserID != jiaUserID {
		return c.String(http.StatusNotFound, "not found: isu")
	}

	res, err := generateIsuGraphResponse(jiaIsuUUID, date)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	return c.JSONBlob(http.StatusOK, jsonEncode(res))
}

// グラフのデータ点を一日分生成
func generateIsuGraphResponse(jiaIsuUUID string, startTime time.Time) ([]GraphResponse, error) {
	endTime := startTime.Add(time.Hour * 24)

	dataPoints := make([]GraphDataPointWithInfo, 0, 24)
	conditionsInThisHour := []IsuCondition{}
	timestampsInThisHour := []int64{}
	var startTimeInThisHour time.Time
	var conditions []IsuCondition

	err := dbSelect(&conditions, "SELECT * FROM `isu_condition` WHERE `jia_isu_uuid` = ? AND `timestamp` >= ? AND `timestamp` < ? ORDER BY `timestamp`", jiaIsuUUID, startTime, endTime)
	if err != nil {
		return nil, fmt.Errorf("db error: %v", err)
	}

	for _, condition := range conditions {
		truncatedConditionTime := condition.Timestamp.Truncate(time.Hour)
		if truncatedConditionTime != startTimeInThisHour {
			if len(conditionsInThisHour) > 0 {
				data, err := calculateGraphDataPoint(conditionsInThisHour)
				if err != nil {
					return nil, err
				}

				dataPoints = append(dataPoints,
					GraphDataPointWithInfo{
						JIAIsuUUID:          jiaIsuUUID,
						StartAt:             startTimeInThisHour,
						Data:                data,
						ConditionTimestamps: timestampsInThisHour})
			}

			startTimeInThisHour = truncatedConditionTime
			conditionsInThisHour = []IsuCondition{}
			timestampsInThisHour = []int64{}
		}
		conditionsInThisHour = append(conditionsInThisHour, condition)
		timestampsInThisHour = append(timestampsInThisHour, condition.Timestamp.Unix())
	}

	if len(conditionsInThisHour) > 0 {
		data, err := calculateGraphDataPoint(conditionsInThisHour)
		if err != nil {
			return nil, err
		}

		dataPoints = append(dataPoints,
			GraphDataPointWithInfo{
				JIAIsuUUID:          jiaIsuUUID,
				StartAt:             startTimeInThisHour,
				Data:                data,
				ConditionTimestamps: timestampsInThisHour})
	}

	responseList := []GraphResponse{}
	index := 0
	thisTime := startTime

	for thisTime.Before(endTime) {
		var data *GraphDataPoint
		timestamps := []int64{}

		if index < len(dataPoints) {
			dataWithInfo := dataPoints[index]

			if dataWithInfo.StartAt.Equal(thisTime) {
				data = &dataWithInfo.Data
				timestamps = dataWithInfo.ConditionTimestamps
				index++
			}
		}

		resp := GraphResponse{
			StartAt:             thisTime.Unix(),
			EndAt:               thisTime.Add(time.Hour).Unix(),
			Data:                data,
			ConditionTimestamps: timestamps,
		}
		responseList = append(responseList, resp)

		thisTime = thisTime.Add(time.Hour)
	}

	return responseList, nil
}

// 複数のISUのコンディションからグラフの一つのデータ点を計算
func calculateGraphDataPoint(isuConditions []IsuCondition) (GraphDataPoint, error) {
	isBrokenCount := 0
	isDirtyCount := 0
	isOverweightCount := 0
	rawScore := 0
	sittingCount := 0
	for _, condition := range isuConditions {
		isBroken := strings.Contains(condition.Condition, "is_broken=true")
		isDirty := strings.Contains(condition.Condition, "is_dirty=true")
		isOverweight := strings.Contains(condition.Condition, "is_overweight=true")
		if isBroken {
			isBrokenCount++
		}
		if isDirty {
			isDirtyCount++
		}
		if isOverweight {
			isOverweightCount++
		}

		switch condition.ConditionLevel {
		case conditionLevelCritical:
			rawScore += scoreConditionLevelCritical
		case conditionLevelWarning:
			rawScore += scoreConditionLevelWarning
		case conditionLevelInfo:
			rawScore += scoreConditionLevelInfo
		default:
			return GraphDataPoint{}, fmt.Errorf("invalid condition level: %v", condition.ConditionLevel)
		}

		if condition.IsSitting {
			sittingCount++
		}
	}

	isuConditionsLength := len(isuConditions)

	score := rawScore * 100 / 3 / isuConditionsLength

	sittingPercentage := sittingCount * 100 / isuConditionsLength
	isBrokenPercentage := isBrokenCount * 100 / isuConditionsLength
	isOverweightPercentage := isOverweightCount * 100 / isuConditionsLength
	isDirtyPercentage := isDirtyCount * 100 / isuConditionsLength

	dataPoint := GraphDataPoint{
		Score: score,
		Percentage: ConditionsPercentage{
			Sitting:      sittingPercentage,
			IsBroken:     isBrokenPercentage,
			IsOverweight: isOverweightPercentage,
			IsDirty:      isDirtyPercentage,
		},
	}
	return dataPoint, nil
}

// GET /api/condition/:jia_isu_uuid
// ISUのコンディションを取得
func getIsuConditions(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	jiaIsuUUID := c.Param("jia_isu_uuid")
	if jiaIsuUUID == "" {
		return c.String(http.StatusBadRequest, "missing: jia_isu_uuid")
	}

	endTimeInt64, err := strconv.ParseInt(c.QueryParam("end_time"), 10, 64)
	if err != nil {
		return c.String(http.StatusBadRequest, "bad format: end_time")
	}
	endTime := time.Unix(endTimeInt64, 0)
	conditionLevelCSV := c.QueryParam("condition_level")
	if conditionLevelCSV == "" {
		return c.String(http.StatusBadRequest, "missing: condition_level")
	}
	conditionLevel := strings.Split(conditionLevelCSV, ",")
	slices.Sort(conditionLevel)
	slices.Compact(conditionLevel)

	startTimeStr := c.QueryParam("start_time")
	var startTime time.Time
	if startTimeStr != "" {
		startTimeInt64, err := strconv.ParseInt(startTimeStr, 10, 64)
		if err != nil {
			return c.String(http.StatusBadRequest, "bad format: start_time")
		}
		startTime = time.Unix(startTimeInt64, 0)
	}

	var isuName string
	//err = dbGet(&isuName,
	//	"SELECT name FROM `isu` WHERE `jia_isu_uuid` = ? AND `jia_user_id` = ?",
	//	jiaIsuUUID, jiaUserID,
	//)
	//if err != nil {
	//	if errors.Is(err, sql.ErrNoRows) {
	//		return c.String(http.StatusNotFound, "not found: isu")
	//	}
	//
	//	c.Logger().Errorf("db error: %v", err)
	//	return c.NoContent(http.StatusInternalServerError)
	//}

	isu, err := cacheIsu.Get(context.Background(), jiaIsuUUID)
	if err != nil {
		if errors.Is(err, errIsuNotFound) {
			return c.String(http.StatusNotFound, "not found: isu")
		}

		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	if isu.JIAUserID != jiaUserID {
		return c.String(http.StatusNotFound, "not found: isu")
	}

	isuName = isu.Name

	conditionsResponse, err := getIsuConditionsFromDB(db, jiaIsuUUID, endTime, conditionLevel, startTime, conditionLimit, isuName)
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	return c.JSONBlob(http.StatusOK, jsonEncode(conditionsResponse))
}

// ISUのコンディションをDBから取得
func getIsuConditionsFromDB(db *sqlx.DB, jiaIsuUUID string, endTime time.Time, conditionLevel []string, startTime time.Time,
	limit int, isuName string) ([]*GetIsuConditionResponse, error) {

	type conditionDataTmp struct {
		Timestamp      time.Time `db:"timestamp"`
		IsSitting      bool      `db:"is_sitting"`
		Condition      string    `db:"condition"`
		ConditionLevel string    `db:"condition_level"`
		Message        string    `db:"message"`
	}
	const getColumn = " `timestamp`, `is_sitting`, `condition`, `condition_level`, `message` "
	conditions := []conditionDataTmp{}
	var err error

	var conditionQuery string
	switch len(conditionLevel) {
	case 0:
		return []*GetIsuConditionResponse{}, nil
	case 1:
		conditionQuery = fmt.Sprintf(" AND `condition_level` = '%v'", conditionLevel[0])
	case 2:
		conditionQuery = fmt.Sprintf(" AND `condition_level` IN ('%v', '%v')", conditionLevel[0], conditionLevel[1])
	case 3:
	}

	if startTime.IsZero() {
		err = dbSelect(&conditions,
			"SELECT "+getColumn+" FROM `isu_condition` WHERE `jia_isu_uuid` = ?"+
				"	AND `timestamp` < ?"+
				conditionQuery+
				"	ORDER BY `timestamp` DESC LIMIT ?",
			jiaIsuUUID, endTime, limit,
		)
	} else {
		err = dbSelect(&conditions,
			"SELECT "+getColumn+" FROM `isu_condition` WHERE `jia_isu_uuid` = ?"+
				"	AND `timestamp` < ?"+
				"	AND ? <= `timestamp`"+
				conditionQuery+
				"	ORDER BY `timestamp` DESC LIMIT ?",
			jiaIsuUUID, endTime, startTime, limit,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("db error: %v", err)
	}

	conditionsResponse := []*GetIsuConditionResponse{}
	for _, c := range conditions {
		// cLevel, err := calculateConditionLevel(c.Condition)
		// if err != nil {
		// 	return nil, fmt.Errorf("conditions format error: %s", c.Condition)
		// }
		// if _, ok := conditionLevel[cLevel]; !ok {
		// 	return nil, fmt.Errorf("conditions where error")
		// }

		data := GetIsuConditionResponse{
			JIAIsuUUID:     jiaIsuUUID,
			IsuName:        isuName,
			Timestamp:      c.Timestamp.Unix(),
			IsSitting:      c.IsSitting,
			Condition:      c.Condition,
			ConditionLevel: c.ConditionLevel,
			Message:        c.Message,
		}
		conditionsResponse = append(conditionsResponse, &data)
	}

	if len(conditionsResponse) > limit {
		conditionsResponse = conditionsResponse[:limit]
	}

	return conditionsResponse, nil
}

// ISUのコンディションの文字列からコンディションレベルを計算
func calculateConditionLevel(condition string) (string, error) {
	var conditionLevel string

	warnCount := strings.Count(condition, "=true")
	switch warnCount {
	case 0:
		conditionLevel = conditionLevelInfo
	case 1, 2:
		conditionLevel = conditionLevelWarning
	case 3:
		conditionLevel = conditionLevelCritical
	default:
		return "", fmt.Errorf("unexpected warn count")
	}

	return conditionLevel, nil
}

// GET /api/trend
// ISUの性格毎の最新のコンディション情報
func getTrend(c echo.Context) error {
	//characterList := []Isu{}
	//err := dbSelect(&characterList, "SELECT `character` FROM `isu` GROUP BY `character`")
	//if err != nil {
	//	c.Logger().Errorf("db error: %v", err)
	//	return c.NoContent(http.StatusInternalServerError)
	//}
	//
	//res := []TrendResponse{}
	//
	//for _, character := range characterList {
	//	isuList := []Isu{}
	//	err = dbSelect(&isuList,
	//		"SELECT * FROM `isu` WHERE `character` = ?",
	//		character.Character,
	//	)
	//	if err != nil {
	//		c.Logger().Errorf("db error: %v", err)
	//		return c.NoContent(http.StatusInternalServerError)
	//	}
	//	characterInfoIsuConditions := []*TrendCondition{}
	//	characterWarningIsuConditions := []*TrendCondition{}
	//	characterCriticalIsuConditions := []*TrendCondition{}
	//	for _, isu := range isuList {
	//		conditions := []IsuCondition{}
	//		err = dbSelect(&conditions,
	//			"SELECT * FROM `isu_condition` WHERE `jia_isu_uuid` = ? ORDER BY timestamp DESC LIMIT 1",
	//			isu.JIAIsuUUID,
	//		)
	//		if err != nil {
	//			c.Logger().Errorf("db error: %v", err)
	//			return c.NoContent(http.StatusInternalServerError)
	//		}
	//
	//		if len(conditions) > 0 {
	//			isuLastCondition := conditions[0]
	//			conditionLevel, err := calculateConditionLevel(isuLastCondition.Condition)
	//			if err != nil {
	//				c.Logger().Error(err)
	//				return c.NoContent(http.StatusInternalServerError)
	//			}
	//			trendCondition := TrendCondition{
	//				ID:        isu.ID,
	//				Timestamp: isuLastCondition.Timestamp.Unix(),
	//			}
	//			switch conditionLevel {
	//			case "info":
	//				characterInfoIsuConditions = append(characterInfoIsuConditions, &trendCondition)
	//			case "warning":
	//				characterWarningIsuConditions = append(characterWarningIsuConditions, &trendCondition)
	//			case "critical":
	//				characterCriticalIsuConditions = append(characterCriticalIsuConditions, &trendCondition)
	//			}
	//		}
	//
	//	}
	//
	//	sort.Slice(characterInfoIsuConditions, func(i, j int) bool {
	//		return characterInfoIsuConditions[i].Timestamp > characterInfoIsuConditions[j].Timestamp
	//	})
	//	sort.Slice(characterWarningIsuConditions, func(i, j int) bool {
	//		return characterWarningIsuConditions[i].Timestamp > characterWarningIsuConditions[j].Timestamp
	//	})
	//	sort.Slice(characterCriticalIsuConditions, func(i, j int) bool {
	//		return characterCriticalIsuConditions[i].Timestamp > characterCriticalIsuConditions[j].Timestamp
	//	})
	//	res = append(res,
	//		TrendResponse{
	//			Character: character.Character,
	//			Info:      characterInfoIsuConditions,
	//			Warning:   characterWarningIsuConditions,
	//			Critical:  characterCriticalIsuConditions,
	//		})
	//}

	res, err := trendDataCache.Get(context.Background(), struct{}{})
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	return c.JSONBlob(http.StatusOK, jsonEncode(res))
}

var trendDataCache = sc.NewMust(getTrendData, 0, 500*time.Millisecond)

func getTrendData(_ context.Context, _ struct{}) ([]*TrendResponse, error) {
	characterList := []Isu{}
	err := dbSelect(&characterList, "SELECT `character` FROM `isu` GROUP BY `character`")
	if err != nil {
		return nil, err
	}

	type latestConditionData struct {
		IsuId          int       `db:"isu_id"`
		Character      string    `db:"character"`
		Timestamp      time.Time `db:"timestamp"`
		ConditionLevel string    `db:"condition_level"`
	}

	lastConditions := []latestConditionData{}
	query := "SELECT i.id AS isu_id, `character`, timestamp, `condition_level` FROM isu_condition AS cond " +
		"JOIN isu AS i ON i.jia_isu_uuid = cond.jia_isu_uuid " +
		"WHERE (cond.jia_isu_uuid, timestamp) IN (SELECT jia_isu_uuid, MAX(timestamp) FROM isu_condition GROUP BY jia_isu_uuid) " +
		"ORDER BY timestamp DESC"
	err = dbSelect(&lastConditions, query)
	if err != nil {
		return nil, err
	}

	perCharacter := map[string]*TrendResponse{}
	for _, character := range characterList {
		perCharacter[character.Character] = &TrendResponse{
			Character: character.Character,
			Info:      make([]*TrendCondition, 0),
			Warning:   make([]*TrendCondition, 0),
			Critical:  make([]*TrendCondition, 0),
		}
	}

	for _, condition := range lastConditions {
		trendCondition := &TrendCondition{
			ID:        condition.IsuId,
			Timestamp: condition.Timestamp.Unix(),
		}

		res := perCharacter[condition.Character]
		switch condition.ConditionLevel {
		case conditionLevelInfo:
			res.Info = append(res.Info, trendCondition)
		case conditionLevelWarning:
			res.Warning = append(res.Warning, trendCondition)
		case conditionLevelCritical:
			res.Critical = append(res.Critical, trendCondition)
		}
	}

	responses := make([]*TrendResponse, 0, len(characterList))
	for _, character := range characterList {
		res := perCharacter[character.Character]
		responses = append(responses, res)
	}

	return responses, nil
}

type conditionInsertDatum struct {
	JiaIsuUUID     string    `db:"jia_isu_uuid"`
	Timestamp      time.Time `db:"timestamp"`
	IsSitting      bool      `db:"is_sitting"`
	Condition      string    `db:"condition"`
	ConditionLevel string    `db:"condition_level"`
	Message        string    `db:"message"`
}

var conditionsLock sync.Mutex
var conditionsQueue []*conditionInsertDatum

var insertConditionThrottler = sc.NewMust(func(ctx context.Context, _ struct{}) (struct{}, error) {
	conditionsLock.Lock()
	toInsert := conditionsQueue
	conditionsQueue = make([]*conditionInsertDatum, 0, len(toInsert))
	conditionsLock.Unlock()

	if len(toInsert) == 0 {
		return struct{}{}, nil
	}

	query := "INSERT INTO `isu_condition` (`jia_isu_uuid`, `timestamp`, `is_sitting`, `condition`, `condition_level`, `message`) VALUES (:jia_isu_uuid, :timestamp, :is_sitting, :condition, :condition_level, :message)"
	_, err := db.NamedExec(query, toInsert)
	if err != nil {
		log.Errorf("condition batch insert db error: %v\n", err)
		return struct{}{}, err
	}
	return struct{}{}, nil
}, 0, 0, sc.EnableStrictCoalescing())

var errIsuNotFound = errors.New("isu not found")

func retrieveIsu(_ context.Context, jiaIsuUUID string) (*Isu, error) {
	var isu Isu
	err := dbGet(&isu, "SELECT * FROM `isu` WHERE `jia_isu_uuid` = ?", jiaIsuUUID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errIsuNotFound
		}
		return nil, err
	}

	return &isu, nil
}

var cacheIsu = sc.NewMust[string, *Isu](retrieveIsu, 300*time.Hour, 300*time.Hour)

// POST /api/condition/:jia_isu_uuid
// ISUからのコンディションを受け取る
func postIsuCondition(c echo.Context) error {
	// TODO: 一定割合リクエストを落としてしのぐようにしたが、本来は全量さばけるようにすべき
	// dropProbability := 0.9
	// if rand.Float64() <= dropProbability {
	// 	//c.Logger().Warnf("drop post isu condition request")
	// 	return c.NoContent(http.StatusAccepted)
	// }

	jiaIsuUUID := c.Param("jia_isu_uuid")
	if jiaIsuUUID == "" {
		return c.String(http.StatusBadRequest, "missing: jia_isu_uuid")
	}

	req := []PostIsuConditionRequest{}
	err := c.Bind(&req)
	if err != nil {
		return c.String(http.StatusBadRequest, "bad request body")
	} else if len(req) == 0 {
		return c.String(http.StatusBadRequest, "bad request body")
	}

	//var count int
	//err = dbGet(&count, "SELECT COUNT(*) FROM `isu` WHERE `jia_isu_uuid` = ?", jiaIsuUUID)
	//if err != nil {
	//	c.Logger().Errorf("db error: %v", err)
	//	return c.NoContent(http.StatusInternalServerError)
	//}
	//if count == 0 {
	//	return c.String(http.StatusNotFound, "not found: isu")
	//}

	_, err = cacheIsu.Get(context.Background(), jiaIsuUUID)
	if err != nil {
		if errors.Is(err, errIsuNotFound) {
			return c.String(http.StatusNotFound, "not found: isu")
		}
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	//for _, cond := range req {
	//	timestamp := time.Unix(cond.Timestamp, 0)
	//
	//	if !isValidConditionFormat(cond.Condition) {
	//		return c.String(http.StatusBadRequest, "bad request body")
	//	}
	//
	//	_, err = tx.Exec(
	//		"INSERT INTO `isu_condition`"+
	//			"	(`jia_isu_uuid`, `timestamp`, `is_sitting`, `condition`, `message`)"+
	//			"	VALUES (?, ?, ?, ?, ?)",
	//		jiaIsuUUID, timestamp, cond.IsSitting, cond.Condition, cond.Message)
	//	if err != nil {
	//		c.Logger().Errorf("db error: %v", err)
	//		return c.NoContent(http.StatusInternalServerError)
	//	}
	//
	//}

	data := make([]*conditionInsertDatum, 0, len(req))
	for _, cond := range req {
		timestamp := time.Unix(cond.Timestamp, 0)

		if !isValidConditionFormat(cond.Condition) {
			return c.String(http.StatusBadRequest, "bad request body")
		}
		cLevel, err := calculateConditionLevel(cond.Condition)
		if err != nil {
			return c.String(http.StatusBadRequest, "bad request body")
		}

		data = append(data, &conditionInsertDatum{
			JiaIsuUUID:     jiaIsuUUID,
			Timestamp:      timestamp,
			IsSitting:      cond.IsSitting,
			Condition:      cond.Condition,
			ConditionLevel: cLevel,
			Message:        cond.Message,
		})
	}

	go func() {
		conditionsLock.Lock()
		conditionsQueue = append(conditionsQueue, data...)
		if len(conditionsQueue) > 10000 {
			insertConditionThrottler.Purge() // immediately initiate next call
		}
		conditionsLock.Unlock()
		_, _ = insertConditionThrottler.Get(context.Background(), struct{}{})
	}()

	return c.NoContent(http.StatusAccepted)
}

// ISUのコンディションの文字列がcsv形式になっているか検証
func isValidConditionFormat(conditionStr string) bool {

	keys := []string{"is_dirty=", "is_overweight=", "is_broken="}
	const valueTrue = "true"
	const valueFalse = "false"

	idxCondStr := 0

	for idxKeys, key := range keys {
		if !strings.HasPrefix(conditionStr[idxCondStr:], key) {
			return false
		}
		idxCondStr += len(key)

		if strings.HasPrefix(conditionStr[idxCondStr:], valueTrue) {
			idxCondStr += len(valueTrue)
		} else if strings.HasPrefix(conditionStr[idxCondStr:], valueFalse) {
			idxCondStr += len(valueFalse)
		} else {
			return false
		}

		if idxKeys < (len(keys) - 1) {
			if conditionStr[idxCondStr] != ',' {
				return false
			}
			idxCondStr++
		}
	}

	return (idxCondStr == len(conditionStr))
}

func getIndex(c echo.Context) error {
	c.Response().Header().Add("Cache-Control", "public, max-age=86400")
	return c.File(frontendContentsPath + "/index.html")
}
