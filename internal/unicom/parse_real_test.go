package unicom

import (
	"testing"

	"chinamobile-monitor/internal/store"

	"github.com/tidwall/gjson"
)

// 真实响应回归测试（2026-09-10 抓取，敏感字段已裁剪）：
// 联通王卡（新）用户，通用 20GB / 专属 30GB，账户余额 38.00 元。
// 数值为抓取时刻真值：通用已用 752.64MB、专属已用 2395.28MB、总已用 3147.90MB。

const realFlowLeftJSON = `{
 "onlineCustomerServiceSwitch": false,
 "newTwFlag": "1",
 "chargingCapacityLink": "<trimmed>",
 "buyVoiceLink": "<trimmed>",
 "packageID": "91215527",
 "canUseSmsAll": "0",
 "RzbResources": [
  {
   "details": [],
   "rzbAllUse": "0.0"
  }
 ],
 "useDailyPercent": "0",
 "unlimitExclusiveFeeIdSet": [],
 "Topblock": "0",
 "flowTypeTwoDesc": "<trimmed>",
 "XsbResources": [
  {
   "details": []
  }
 ],
 "flowSumList": [
  {
   "elemtype": "3",
   "flowtype": "1",
   "xcanusevalue": "19727.36",
   "xusedvalue": "752.64"
  },
  {
   "elemtype": "3",
   "flowtype": "2",
   "xcanusevalue": "28324.72",
   "xusedvalue": "2395.28"
  },
  {
   "elemtype": "3",
   "flowtype": "3",
   "xcanusevalue": "0.00",
   "xusedvalue": "0.12"
  }
 ],
 "rzbAllUse": "0.0",
 "csImg": "<trimmed>",
 "languageflag": "0",
 "sumPercent": "76",
 "hasOtherTypeFlowExceptRzb": "1",
 "MlResources": [
  {
   "details": [
    {
     "elemType": "3",
     "endDate": "",
     "endXsbDate": "",
     "feePolicyId": "10047",
     "feePolicyName": "腾讯游戏",
     "flowType": "3",
     "hide": false,
     "hideCarryForwardLabel": false,
     "hideUnsharedLabel": false,
     "limited": "1",
     "use": "0.12",
     "usedPercent": "0"
    }
   ],
   "type": "MlFlowdetailsList",
   "userResource": "0.12"
  }
 ],
 "canuseVoiceAllUnit": "分钟",
 "crowdfundingFlag": true,
 "smsHeadUsed": 0,
 "allUserFlow": "3147.90",
 "TwResources": [
  {
   "details": [
    {
     "addUpItemName": "套外语音",
     "endDate": "",
     "endXsbDate": "",
     "feePolicyName": "联通王卡(新)主资费",
     "hide": false,
     "hideCarryForwardLabel": false,
     "hideUnsharedLabel": false,
     "limited": "0",
     "rbFlag": "0",
     "remain": "0",
     "resourceSource": "0",
     "resourceType": "02",
     "total": "0",
     "twFlowDisplayTypeIdentification": "0",
     "typemark": "1",
     "use": "0",
     "usedPercent": "0",
     "xexceedvalue": "16"
    }
   ],
   "outResource": "16",
   "type": "Voice",
   "userResource": "16"
  }
 ],
 "canUseFlowAll": "19.27",
 "flowTypeOneDesc": "<trimmed>",
 "feePolicyIds": "72271285,75606696",
 "accountBAR": [
  {
   "businessCode": "10026",
   "buttonName": "充流量",
   "createTime": "2021-02-20 16:42:17",
   "creator": "YH_songnazb",
   "creatorProvinceCode": "098",
   "id": "8dd4df06-3e59-4d9b-9ed4-530d407e33bb",
   "linkUrl": "<trimmed>",
   "nettype": "01;02;03;04;05;07;08;09;10;11;12;13;14;99",
   "orderNumber": "210",
   "paymentType": "1;2",
   "province": "051",
   "status": "00",
   "userId": ""
  }
 ],
 "subscribeToTextMessagesLink": "<trimmed>",
 "voiceExceed": 16,
 "code": "0000",
 "canuseFlowAllUnit": "GB",
 "sumresource": 3147.9,
 "fresSumList": [
  {
   "elemtype": "3",
   "flowtype": "1",
   "xcanusevalue": "19727.36",
   "xexceedvalue": "0.00",
   "xusedvalue": "752.64"
  },
  {
   "elemtype": "3",
   "flowtype": "2",
   "xcanusevalue": "28324.72",
   "xexceedvalue": "0.00",
   "xusedvalue": "2395.28"
  },
  {
   "elemtype": "3",
   "flowtype": "3",
   "xcanusevalue": "0.00",
   "xexceedvalue": "0.00",
   "xusedvalue": "0.12"
  }
 ],
 "flowTypeFourDesc": "<trimmed>",
 "cityCode": "510",
 "flowTypeThreeDesc": "<trimmed>",
 "sum": "3148.04",
 "usedVoiceNounExplain": "<trimmed>",
 "csEntranceUrl": "<trimmed>",
 "flowExceed": 0.0,
 "balancesumqry": "1",
 "wangTTcUrl": "<trimmed>",
 "usePercent": [
  {
   "Value": "76",
   "value": "76"
  },
  {
   "Value": "0",
   "value": "0"
  },
  {
   "Value": "0",
   "value": "0"
  },
  {
   "Value": "24",
   "value": "24"
  }
 ],
 "voiceSumresource": 0,
 "packageName": "联通王卡（新）",
 "wangTkfUrl": "<trimmed>",
 "otherTypeFlowSum": {
  "$ref": "$.fresSumList[2]"
 },
 "canUseValueAll": "0",
 "summary": {
  "domesticDailyFlow": "0.00",
  "fengdingstate": "0",
  "freeFlow": "2395.40",
  "freePercent": "1",
  "isSum": "0",
  "percent": [
   {
    "Value": "1",
    "value": "1"
   },
   {
    "Value": "98",
    "value": "98"
   },
   {
    "Value": "1",
    "value": "1"
   }
  ],
  "remainFengDing": "201652",
  "remainPercent": "98",
  "sum": "3148.04",
  "sumfengDing": "200",
  "sumresource": "3148.04",
  "usePercent": "1"
 },
 "s1HistoryFlowDetails": [],
 "reminder": "<trimmed>",
 "provinceCode": "051",
 "packageId": "91215527",
 "usefreePercent": "76",
 "mobile": "<trimmed>",
 "resources": [
  {
   "details": [
    {
     "addUpItemName": "套餐内专享免费流量(30.00G)",
     "addupItemCode": "40008",
     "elemType": "3",
     "endDate": "长期有效",
     "endDate1": "2029-12-31 23:59:59",
     "endXsbDate": "长期有效",
     "feePolicyId": "72271285",
     "feePolicyName": "30GB联通王卡(新)专属流量包",
     "flowType": "2",
     "hide": false,
     "hideCarryForwardLabel": false,
     "hideUnsharedLabel": false,
     "limited": "0",
     "rbFlag": "0",
     "realresourcetype": "13",
     "remain": "28324.72",
     "resourceSource": "0",
     "resourceType": "13",
     "total": "30720.00",
     "typemark": "1",
     "use": "2395.27",
     "usedPercent": "8",
     "xexceedvalue": "0.00"
    },
    {
     "addUpItemName": "套内国内流量(20.00G)",
     "addupItemCode": "40025",
     "elemType": "3",
     "endDate": "长期有效",
     "endDate1": "2029-12-31 23:59:59",
     "endXsbDate": "长期有效",
     "feePolicyId": "75606696",
     "feePolicyName": "广东联通王卡专属10元资源包（20GB通用流量）（次月生效）",
     "flowType": "1",
     "hide": false,
     "hideCarryForwardLabel": false,
     "hideUnsharedLabel": false,
     "limited": "0",
     "rbFlag": "0",
     "realresourcetype": "01",
     "remain": "19727.36",
     "resourceSource": "0",
     "resourceType": "01",
     "total": "20480.00",
     "typemark": "1",
     "use": "752.63",
     "usedPercent": "4",
     "xexceedvalue": "0.00"
    }
   ],
   "remainResource": "48052.08",
   "type": "flow",
   "url": "<trimmed>",
   "userResource": "3147.9",
   "wTurl": "<trimmed>",
   "wangTurl": "<trimmed>"
  },
  {
   "details": [],
   "remainResource": "0",
   "type": "Voice",
   "url": "<trimmed>",
   "userResource": "0",
   "wTurl": "<trimmed>",
   "wangTurl": "<trimmed>"
  },
  {
   "details": [],
   "remainResource": "0",
   "type": "smsList",
   "url": "<trimmed>",
   "userResource": "0",
   "wTurl": "<trimmed>",
   "wangTurl": "<trimmed>"
  }
 ],
 "trafficPromptsAreExempted": "<trimmed>",
 "leftMonth": 8,
 "canuseSmsAllUnit": "条",
 "voiceHeadUsed": 16,
 "s2HistoryFlowDetails": [],
 "time": "2026-09-10 17:04:11",
 "businessType": "4g",
 "MlProgrammeFlag": "A",
 "smsSumresource": 0,
 "balancedetailqry": "1",
 "newVersion": true,
 "smsExceed": 0
}`

const realBalanceJSON = `{
 "curntbalancecust": "38.00",
 "totalrealfee": "31.27",
 "realfeecustnew": "21.27",
 "allbillfee": "31.27",
 "code": "0000",
 "msg": ""
}`

func TestParseFlowLeftRealResponse(t *testing.T) {
	r := ParseFlowLeft(gjson.Parse(realFlowLeftJSON))
	if r.PlanName != "联通王卡（新）" {
		t.Errorf("套餐名 = %q", r.PlanName)
	}
	// flowSumList：type1 通用 752.64/19727.36 MB，type2 专属 2395.28/28324.72 MB
	if r.GeneralFlow.Used != "0.73GB" || r.GeneralFlow.Total != "20GB" {
		t.Errorf("通用流量 = %s/%s", r.GeneralFlow.Used, r.GeneralFlow.Total)
	}
	if r.SpecialFlow.Used != "2.34GB" || r.SpecialFlow.Total != "30GB" {
		t.Errorf("专属流量 = %s/%s", r.SpecialFlow.Used, r.SpecialFlow.Total)
	}
	if r.RegionalFlow.Used != "0.12MB" {
		t.Errorf("其他流量已用 = %s", r.RegionalFlow.Used)
	}
	// allUserFlow 3147.90MB 已用；总量取三类合计 51200.12MB = 50GB
	if r.TotalFlow.Used != "3.07GB" || r.TotalFlow.Total != "50GB" {
		t.Errorf("总流量 = %s/%s", r.TotalFlow.Used, r.TotalFlow.Total)
	}
	if r.Voice.Used != "16分钟" {
		t.Errorf("语音已用 = %s", r.Voice.Used)
	}
	if r.FlowUsedPercent < 6.14 || r.FlowUsedPercent > 6.16 {
		t.Errorf("流量已用百分比 = %.2f", r.FlowUsedPercent)
	}
}

func TestParseBalanceRealResponse(t *testing.T) {
	r := &store.QueryResult{}
	ParseBalance(r, gjson.Parse(realBalanceJSON))
	if r.Balance != "38.00元" || r.BalanceNum != 38 {
		t.Errorf("余额 = %s (%v)", r.Balance, r.BalanceNum)
	}
	if r.RealtimeFee != "31.27" {
		t.Errorf("实时话费 = %s", r.RealtimeFee)
	}
	if r.BillTotal != "31.27" {
		t.Errorf("本月账单 = %s", r.BillTotal)
	}
}
