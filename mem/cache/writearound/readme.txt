1.  coalescer
    request를 받으면 work group?에서 마지막 instruction인지, coalesce가 가능한지 확인
    type에 따라 coalesce, send 수행
    coalesce:   toCoalesce 리스트에 해당 transaction 추가
    send:   toCoalesce[0]에 해당하는 transaction을 dirBuf에 추가함 -> directory stage에서 처리됨
            toCoalesce는 nil로 처리하고 postCoalesceTransactions에 dirBuf에 추가한 transaction 저장

2.  directory
    pipeline이 full인지 확인하고 dirBuf의 transaction을 pipeline에 push

    buf의 item을 확인하여 read/write 수행
    read:   MSHR hit인 경우, mshrEntry.request에 transaction 추가, 종료
            
            read hit인 경우 해당 block이 lock인지 확인, bankBuf가 full인지 확인
            transaction의 bankAction을 readhit으로 기록하고, bankBuf에 추가한 후 종료

            read miss인 경우, victim과 MSHR full 여부를 확인함
            read 요청 전송, mshrEntry에 기록

    write:  MSHR hit인 경우, write request를 bottomPort으로 전송, mshrEntry에 transaction 추가, 종료

            write hit인 경우, block의 write 가능 여부를 확인, bankBuf가 full인지 확인
            transaction의 bankAction을 write로 기록하고 bankBuf에 push, 종료

            write miss인 경우, write request를 bottomPort으로 전송, 종료

2.1 bottomparser
    write done response인 경우
    write response에 해당하는 transaction들을 완료처리
    transaction을 cache의 postCoalesceTransactions에서 제거

    data ready response인 경우
    read response에 해당하는 transaction 확인, 해당하는 mshr entry를 제거
    bankBuf가 full인지 확인하고 transaction의 bankAction을 writefetched로 기록하고 push
    transaction을 cache의 poseCoalesceTransactions에서 제거

3.  bankstage
    bankBuf에서 item 확인, pipeline이 full인지 확인하고 pipeline에 item push

    bankAction이 read hit인 경우(read hit),
    postPipelineBuf에서 item 확인
    item에 해당하는 transaction에 대해 완료기록, cache의 poseCoalesceTransactions에서 제거

    bankAction이 write인 경우(write hit),
    write 수행
    완료 표시를 안 하나???

    bankAction이 write fetched인 경우(read response),
    write 수행, 종료
    완료 표시 없음??

4.  respondstage
    각 transaction에 대해 반복
    trans.done이 true인 경우 respondRead/respondWrite 수행
    
    respondRead
        read response를 top으로 전송
        cache transaction에서 삭제

    respondWrite   
        write response를 top으로 전송
        cache transaction에서 삭제