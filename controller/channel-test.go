package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"net/http"
	"one-api/common"
	"one-api/model"
	"strconv"
	"sync"
	"time"
)

func testChannel(channel *model.Channel, request ChatRequest) error {
	switch channel.Type {
	case common.ChannelTypeAzure:
		request.Model = "gpt-35-turbo"
	default:
		request.Model = "gpt-3.5-turbo"
	}
	requestURL := common.ChannelBaseURLs[channel.Type]
	if channel.Type == common.ChannelTypeAzure {
		requestURL = fmt.Sprintf("%s/openai/deployments/%s/chat/completions?api-version=2023-03-15-preview", channel.BaseURL, request.Model)
	} else {
		if channel.Type == common.ChannelTypeCustom {
			requestURL = channel.BaseURL
		} else if channel.Type == common.ChannelTypeOpenAI && channel.BaseURL != "" {
			requestURL = channel.BaseURL
		}
		requestURL += "/v1/chat/completions"
	}

	jsonData, err := json.Marshal(request)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", requestURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return err
	}
	if channel.Type == common.ChannelTypeAzure {
		req.Header.Set("api-key", channel.Key)
	} else {
		req.Header.Set("Authorization", "Bearer "+channel.Key)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var response TextResponse
	err = json.NewDecoder(resp.Body).Decode(&response)
	if err != nil {
		return err
	}
	if response.Usage.CompletionTokens == 0 {
		return errors.New(fmt.Sprintf("type %s, code %v, message %s", response.Error.Type, response.Error.Code, response.Error.Message))
	}
	return nil
}

func buildTestRequest(c *gin.Context) *ChatRequest {
	model_ := c.Query("model")
	testRequest := &ChatRequest{
		Model:     model_,
		MaxTokens: 1,
	}
	testMessage := Message{
		Role:    "user",
		Content: "hi",
	}
	testRequest.Messages = append(testRequest.Messages, testMessage)
	return testRequest
}

func TestChannel(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	channel, err := model.GetChannelById(id, true)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	testRequest := buildTestRequest(c)
	tik := time.Now()
	err = testChannel(channel, *testRequest)
	tok := time.Now()
	milliseconds := tok.Sub(tik).Milliseconds()
	go channel.UpdateResponseTime(milliseconds)
	consumedTime := float64(milliseconds) / 1000.0
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
			"time":    consumedTime,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"time":    consumedTime,
	})
	return
}

var testAllChannelsLock sync.Mutex
var testAllChannelsRunning bool = false

// disable & notify
func disableChannel(channelId int, channelName string, reason string) {
	if common.RootUserEmail == "" {
		common.RootUserEmail = model.GetRootUserEmail()
	}
	model.UpdateChannelStatusById(channelId, common.ChannelStatusDisabled)
	subject := fmt.Sprintf("通道「%s」（#%d）已被禁用", channelName, channelId)
	content := fmt.Sprintf("通道「%s」（#%d）已被禁用，原因：%s", channelName, channelId, reason)
	err := common.SendEmail(subject, common.RootUserEmail, content)
	if err != nil {
		common.SysError(fmt.Sprintf("发送邮件失败：%s", err.Error()))
	}
}

func testAllChannels(c *gin.Context) error {
	if common.RootUserEmail == "" {
		common.RootUserEmail = model.GetRootUserEmail()
	}
	testAllChannelsLock.Lock()
	if testAllChannelsRunning {
		testAllChannelsLock.Unlock()
		return errors.New("测试已在运行中")
	}
	testAllChannelsRunning = true
	testAllChannelsLock.Unlock()
	channels, err := model.GetAllChannels(0, 0, true)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return err
	}
	testRequest := buildTestRequest(c)
	var disableThreshold = int64(common.ChannelDisableThreshold * 1000)
	if disableThreshold == 0 {
		disableThreshold = 10000000 // a impossible value
	}
	go func() {
		for _, channel := range channels {
			if channel.Status != common.ChannelStatusEnabled {
				continue
			}
			tik := time.Now()
			err := testChannel(channel, *testRequest)
			tok := time.Now()
			milliseconds := tok.Sub(tik).Milliseconds()
			if err != nil || milliseconds > disableThreshold {
				if milliseconds > disableThreshold {
					err = errors.New(fmt.Sprintf("响应时间 %.2fs 超过阈值 %.2fs", float64(milliseconds)/1000.0, float64(disableThreshold)/1000.0))
				}
				disableChannel(channel.Id, channel.Name, err.Error())
			}
			channel.UpdateResponseTime(milliseconds)
		}
		err := common.SendEmail("通道测试完成", common.RootUserEmail, "通道测试完成，如果没有收到禁用通知，说明所有通道都正常")
		if err != nil {
			common.SysError(fmt.Sprintf("发送邮件失败：%s", err.Error()))
		}
		testAllChannelsLock.Lock()
		testAllChannelsRunning = false
		testAllChannelsLock.Unlock()
	}()
	return nil
}

func TestAllChannels(c *gin.Context) {
	err := testAllChannels(c)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
	})
	return
}
