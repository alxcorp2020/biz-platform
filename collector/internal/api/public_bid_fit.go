package api

import "strings"

// public_bid_fit — 2026-08-08. 공공입찰(procurement)과 사실상 무관한 B2C 업종
// (음식점·카페·미용실 등)을 "가입은 허용하되 공공입찰 추천이 매우 적을 수 있음"을
// 안내하기 위한 판별. 조달청 공공조달분류는 "정부에 무엇을 납품/용역하는가" 기준이라
// 일반 소비자 대상 소매·요식·생활서비스는 어느 분류에도 맞지 않는다 — 이들이 온보딩
// 필수 "업종"에서 막히거나 틀린 분류를 억지로 고르지 않도록 명시적 선택지
// (consumerRetailIndustry)를 주고, 업태 키워드로 자동 감지해 경고 배너를 띄운다.

// consumerRetailIndustry — 온보딩에서 조달청 37개 분류 어디에도 맞지 않는 일반
// 소비자 대상(B2C) 업종이 고를 수 있는 명시적 선택지 값. "기타"와 달리 (1) 이름이
// 정직하고 (2) 공공입찰 저관련 안내의 신호로 쓴다. notices.industry엔 이 값이 절대
// 없으므로 scoreIndustry가 자연히 어떤 공고와도 매칭하지 않는다(공공입찰 추천 0건).
const consumerRetailIndustry = "일반 소비자 대상 업종"

// publicBidFit 3단계 — 프론트가 이 값으로 경고 배너 노출/톤을 정한다.
const (
	publicBidFitNone   = "none"   // 공공입찰이 매우 적음(음식/카페/주점/노래방/미용/세탁 등 B2C)
	publicBidFitLow    = "low"    // 공고가 적은 편(편의점/소매/의류/꽃집/문구/안경/애견 등)
	publicBidFitNormal = "normal" // 일반(경고 없음)
)

// 업태(business_type) 키워드 — 사업자등록증 OCR/직접기재의 업태 원문에서 찾는다.
// 상호명은 자유형식이라 오탐 위험이 커 넣지 않는다(feedback_korean_ilike_keyword_false_positive
// 참고). 키워드는 오탐이 적도록 되도록 3글자 이상·업태 특유의 단어로 고른다.
var publicBidFitNoneKeywords = []string{
	"음식점", "일반음식점", "휴게음식점", "한식", "중식", "일식", "양식", "분식", "식당",
	"카페", "커피", "다방",
	"주점", "호프", "술집", "포차", "유흥", "단란",
	"노래방", "노래연습장",
	"피시방", "피씨방", "pc방", "인터넷컴퓨터게임",
	"당구장",
	"미용실", "미용업", "이용업", "헤어", "네일", "피부관리", "에스테틱", "피부미용",
	"세탁소", "세탁업",
}

var publicBidFitLowKeywords = []string{
	"편의점", "소매", "의류", "꽃집", "화원", "플라워", "문구", "안경", "애견", "반려동물",
}

// classifyPublicBidFit — 회사가 실제 조달청 업종을 하나라도 선택했으면(=공공입찰을
// 한다) 경고하지 않는다("normal"). 명시적으로 consumerRetailIndustry를 골랐으면
// 곧바로 none. 그 외에는 업태 키워드로 none/low를 판별하고, 아무 신호도 없으면
// 잘못된 경고를 피하려 normal(안내 안 함)로 둔다.
func classifyPublicBidFit(businessType, industry []string) string {
	for _, ind := range industry {
		ind = strings.TrimSpace(ind)
		if ind == consumerRetailIndustry {
			return publicBidFitNone
		}
		if ind != "" && ind != "기타" {
			return publicBidFitNormal // 실제 조달청 중분류 선택 → 공공입찰 대상
		}
	}
	hay := strings.ToLower(strings.Join(businessType, " "))
	if hay == "" {
		return publicBidFitNormal
	}
	for _, kw := range publicBidFitNoneKeywords {
		if strings.Contains(hay, strings.ToLower(kw)) {
			return publicBidFitNone
		}
	}
	for _, kw := range publicBidFitLowKeywords {
		if strings.Contains(hay, strings.ToLower(kw)) {
			return publicBidFitLow
		}
	}
	return publicBidFitNormal
}
